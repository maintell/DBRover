param(
  [Parameter(Mandatory=$true)][string]$Engine,
  [Parameter(Mandatory=$true)][int]$Port,
  [Parameter(Mandatory=$true)][string]$User,
  [string]$Password = 'testpass',
  [string]$Database = 'acc'
)
$ErrorActionPreference = 'Stop'


$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$work = Join-Path $root ".tmp\accept-$Engine"
$httpPort = if ($Engine -eq 'mysql') { 7792 } else { 7793 }
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = "acc-$Engine"
$rows = 3000

New-Item -ItemType Directory -Force -Path $work | Out-Null
$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$httpPort", '-token', $token, '-audit', (Join-Path $work 'audit.jsonl')
)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  $connBody = @{
    name = "acc-$Engine"; engine = $Engine; host = '192.168.2.88'; port = $Port
    catalog = $Database; user = $User; password = $Password
    environment = 'test'; access_mode = 'read-write'
  }
  if ($Engine -eq 'postgres') { $connBody.schema = 'public' }
  $conn = Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) `
    -Body ($connBody | ConvertTo-Json)
  $cid = $conn.id
  Write-Host "已连接 $Engine（$($conn.name)）"

  function Exec([string]$sql) {
    $body = @{ sql = $sql } | ConvertTo-Json
    try {
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) -Body $body
    } catch {
      $detail = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($detail.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql; approval_token = $detail.error.approval.token } | ConvertTo-Json)
    }
  }

  Exec 'DROP TABLE IF EXISTS bulk_big' | Out-Null
  Exec 'CREATE TABLE bulk_big (id BIGINT NOT NULL PRIMARY KEY, v VARCHAR(32))' | Out-Null
  # 用生成的 VALUES 造数：MySQL 5.7 没有 WITH RECURSIVE，两种引擎统一走这条。
  $sb = [System.Text.StringBuilder]::new()
  [void]$sb.Append('INSERT INTO bulk_big (id, v) VALUES ')
  for ($i = 1; $i -le $rows; $i++) {
    if ($i -gt 1) { [void]$sb.Append(',') }
    [void]$sb.Append("($i,'x')")
  }
  Exec $sb.ToString() | Out-Null
  Write-Host "造数完成：$rows 行"

  function Plan() {
    Invoke-RestMethod -Uri "$base/connections/$cid/bulk/plan" -Method Post -Headers ($hdr + $json) -Body (@{
        table              = @{ kind = 'table'; name = 'bulk_big' }
        set                = @{ v = 'done' }
        confirm_table      = 'bulk_big'
        accept_no_rollback = $true
      } | ConvertTo-Json -Depth 6)
  }

  function ApplyStream([string]$planId) {
    # 与界面一致：先被 428 拦一次拿凭证，再带凭证流式重提。
    $appr = $null
    try {
      Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) -Body '{}' | Out-Null
    } catch {
      $d = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      $appr = $d.error.approval.token
    }
    $payload = if ($appr) { @{ approval_token = $appr } | ConvertTo-Json } else { '{}' }

    $client = [System.Net.Http.HttpClient]::new()
    $req = [System.Net.Http.HttpRequestMessage]::new('POST', "$base/changes/$planId/apply?stream=1")
    $req.Headers.Add('X-DBRover-Token', $token)
    $req.Content = [System.Net.Http.StringContent]::new($payload, [System.Text.Encoding]::UTF8, 'application/json')
    $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).Result
    $reader = [System.IO.StreamReader]::new($resp.Content.ReadAsStreamAsync().Result)
    $events = @()
    while (-not $reader.EndOfStream) {
      $line = $reader.ReadLine()
      if (-not $line) { continue }
      $ev = $line | ConvertFrom-Json
      $events += , $ev
      if ($ev.type -eq 'progress') { Write-Host "  progress done=$($ev.done)/$($ev.total)" }
      elseif ($ev.type -eq 'result') { Write-Host "  result rows_affected=$($ev.body.rows_affected)" }
      else { Write-Host "  error $($ev.code) $($ev.message)" }
    }
    $reader.Close()
    $client.Dispose()
    return $events
  }

  $p = Plan
  Write-Host ("预案：affected={0} batched={1} batches={2}" -f $p.affected, $p.batched, $p.batches)
  if (-not $p.batched) { throw "$Engine 上 $rows 行应分批" }
  if ($p.batches -ne 3) { throw "batches = $($p.batches)，期望 3" }

  Write-Host '第一次执行（值需要变化）：'
  $ev1 = ApplyStream $p.plan.id
  $prog = @($ev1 | Where-Object { $_.type -eq 'progress' })
  $res = ($ev1 | Where-Object { $_.type -eq 'result' } | Select-Object -Last 1)
  if (-not $res) { throw '没有 result 行' }
  if ($prog.Count -ne 3) { throw "进度行数 = $($prog.Count)，期望 3" }
  if ($res.body.rows_affected -ne $rows) { throw "rows_affected = $($res.body.rows_affected)，期望 $rows" }

  $chk = Exec "SELECT COUNT(*) AS n FROM bulk_big WHERE v = 'done'"
  $n = [int64]$chk.results[0].rows[0][0]
  Write-Host "$Engine 第一次执行后 v='done' 共 $n 行"
  if ($n -ne $rows) { throw "被改行数 = $n，期望 $rows" }

  # 第二次执行：每一行的值已经等于目标值。MySQL 默认把 RowsAffected 报成「实际改动行数」，
  # 没有 clientFoundRows 时这里会每批返回 0、被判成 CHANGE_ROW_CONFLICT。
  Exec "UPDATE bulk_big SET v = 'x'" | Out-Null
  $p2 = Plan
  Write-Host '第二次执行（先全部改回 x，再改回 done，验证不是「同值即冲突」）：'
  $ev2 = ApplyStream $p2.plan.id
  $res2 = ($ev2 | Where-Object { $_.type -eq 'result' } | Select-Object -Last 1)
  if (-not $res2) {
    $err = ($ev2 | Where-Object { $_.type -eq 'error' } | Select-Object -Last 1)
    throw "第二次执行失败：$($err.error.code) $($err.error.message)"
  }
  if ($res2.body.rows_affected -ne $rows) { throw "第二次 rows_affected = $($res2.body.rows_affected)" }

  # 第三次：值已经是 done，不做任何前置改动。这才是真正的「同值」路径。
  $p3 = Plan
  Write-Host '第三次执行（值已经是 done，逐批都是「改成相同的值」）：'
  $ev3 = ApplyStream $p3.plan.id
  $res3 = ($ev3 | Where-Object { $_.type -eq 'result' } | Select-Object -Last 1)
  if (-not $res3) {
    $err = ($ev3 | Where-Object { $_.type -eq 'error' } | Select-Object -Last 1)
    throw "同值批次被判成冲突：$($err.error.code) $($err.error.message)"
  }
  if ($res3.body.rows_affected -ne $rows) {
    throw "同值时 rows_affected = $($res3.body.rows_affected)，期望 $rows（clientFoundRows 生效）"
  }
  Write-Host "$Engine 通过：3 批 × 3 次执行，同值批次未被误判为冲突"

  # 复合主键：键集要按元组推进，批大小自动降到 200
  #（SQLite 的表达式树深度上限 1000，1000 组 OR 会直接报错）。
  Exec 'DROP TABLE IF EXISTS bulk_comp' | Out-Null
  Exec 'CREATE TABLE bulk_comp (a INT NOT NULL, b INT NOT NULL, v VARCHAR(32), PRIMARY KEY (a, b))' | Out-Null
  $sb2 = [System.Text.StringBuilder]::new()
  [void]$sb2.Append('INSERT INTO bulk_comp (a, b, v) VALUES ')
  for ($i = 1; $i -le 1500; $i++) {
    if ($i -gt 1) { [void]$sb2.Append(',') }
    [void]$sb2.Append("($i,$(($i % 3) + 1),'x')")
  }
  Exec $sb2.ToString() | Out-Null

  $pc = Invoke-RestMethod -Uri "$base/connections/$cid/bulk/plan" -Method Post -Headers ($hdr + $json) -Body (@{
      table              = @{ kind = 'table'; name = 'bulk_comp' }
      set                = @{ v = 'done' }
      confirm_table      = 'bulk_comp'
      accept_no_rollback = $true
    } | ConvertTo-Json -Depth 6)
  Write-Host ("  复合主键预案：affected={0} batched={1} batch_size={2} batches={3}" -f `
      $pc.affected, $pc.batched, $pc.batch_size, $pc.batches)
  if (-not $pc.batched) { throw '复合主键应启用分批' }
  if ($pc.batch_size -ne 200) { throw "复合主键批大小 = $($pc.batch_size)，期望 200" }
  $evc = ApplyStream $pc.plan.id
  $resc = ($evc | Where-Object { $_.type -eq 'result' } | Select-Object -Last 1)
  if (-not $resc) {
    $err = ($evc | Where-Object { $_.type -eq 'error' } | Select-Object -Last 1)
    throw "复合主键分批失败：$($err.error.code) $($err.error.message)"
  }
  if ($resc.body.rows_affected -ne 1500) { throw "复合主键 rows_affected = $($resc.body.rows_affected)" }
  $chkc = Exec "SELECT COUNT(*) AS n FROM bulk_comp WHERE v = 'done'"
  if ([int64]$chkc.results[0].rows[0][0] -ne 1500) { throw '复合主键分批后行数不符' }
  Write-Host '  复合主键分批通过：8 批 × 200 行，全部命中'

  Exec 'DROP TABLE bulk_comp' | Out-Null
  Exec 'DROP TABLE bulk_big' | Out-Null
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
