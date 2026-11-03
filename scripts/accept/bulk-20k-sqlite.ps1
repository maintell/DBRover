# 分批批量修改的端到端验收：2 万行、20 批、进度可见、可中止。
# 走真实 HTTP 服务 + 真实 SQLite 引擎，不用单测桩。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$work = Join-Path $root '.tmp\bulk20k'
$db = Join-Path $work 'bulk.db'
$port = 7791
$base = "http://127.0.0.1:$port/api/v1"
$token = 'acc-token-20k'

New-Item -ItemType Directory -Force -Path $work | Out-Null
Remove-Item -Force -ErrorAction SilentlyContinue $db, (Join-Path $work 'audit.jsonl')

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$port", '-token', $token,
  '-audit', (Join-Path $work 'audit.jsonl')
)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }

  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  # 1) 连接 + 建表 + 2 万行造数
  $conn = Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) -Body (@{
      name = 'bulk20k'; engine = 'sqlite'; host = $db
      environment = 'test'; access_mode = 'read-write'
    } | ConvertTo-Json)
  $cid = $conn.id

  function Exec([string]$sql) {
    $body = @{ sql = $sql } | ConvertTo-Json
    try {
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) -Body $body
    } catch {
      $detail = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($detail.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      $tok = $detail.error.approval.token
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql; approval_token = $tok } | ConvertTo-Json)
    }
  }

  Exec "CREATE TABLE big (id INTEGER PRIMARY KEY, v TEXT)" | Out-Null
  Exec "INSERT INTO big (id, v) WITH RECURSIVE seq(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM seq WHERE x < 20000) SELECT x, 'x' FROM seq" | Out-Null
  Write-Output "造数完成：2 万行"

  # 2) 生成预案
  $plan = Invoke-RestMethod -Uri "$base/connections/$cid/bulk/plan" -Method Post -Headers ($hdr + $json) -Body (@{
      table              = @{ kind = 'table'; name = 'big' }
      set                = @{ v = 'done' }
      confirm_table      = 'big'
      accept_no_rollback = $true
    } | ConvertTo-Json -Depth 6)

  Write-Output ("预案：affected={0} batched={1} batch_size={2} batches={3}" -f `
      $plan.affected, $plan.batched, $plan.batch_size, $plan.batches)
  Write-Output ("说明：{0}" -f $plan.note)
  $pid2 = $plan.plan.id

  # 3) 流式执行，逐行打印到达时间（证明是真流，不是攒完一次吐出）
  # 与界面一致：先被 428 拦一次拿到审批凭证，再带凭证重新提交。
  function Approval([string]$planId) {
    try {
      Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) -Body '{}' | Out-Null
      return $null
    } catch {
      $detail = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($detail.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return $detail.error.approval.token
    }
  }

  # cancelAfterMs > 0 时在指定时刻取消整个请求，等同于界面点「停止」（AbortController）。
  # 只「停止读取」是假的：连接还在，服务端会一路跑完——那是测客户端而不是测中止。
  function ApplyStream([string]$planId, [string]$query, [int]$cancelAfterMs = 0) {
    $appr = Approval $planId
    $payload = if ($appr) { @{ approval_token = $appr } | ConvertTo-Json } else { '{}' }
    $client = [System.Net.Http.HttpClient]::new()
    $req = [System.Net.Http.HttpRequestMessage]::new('POST', "$base/changes/$planId/apply$query")
    $req.Headers.Add('X-DBRover-Token', $token)
    $req.Content = [System.Net.Http.StringContent]::new($payload, [System.Text.Encoding]::UTF8, 'application/json')

    $cts = [System.Threading.CancellationTokenSource]::new()
    if ($cancelAfterMs -gt 0) { $cts.CancelAfter($cancelAfterMs) }

    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $events = @()
    try {
      $resp = $client.SendAsync($req, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead, $cts.Token).Result
      $stream = $resp.Content.ReadAsStreamAsync().Result
      $reader = [System.IO.StreamReader]::new($stream)
      while (-not $reader.EndOfStream) {
        $line = $reader.ReadLine()
        if (-not $line) { continue }
        $el = $sw.ElapsedMilliseconds
        $ev = $line | ConvertFrom-Json
        $events += , @{ at = $el; ev = $ev }
        $desc = if ($ev.type -eq 'progress') { "progress done=$($ev.done)/$($ev.total)" }
                elseif ($ev.type -eq 'result') { "result rows_affected=$($ev.body.rows_affected) undo=$($ev.body.undo)" }
                else { "error $($ev.code) $($ev.message)" }
        Write-Host ("  [{0,6} ms] {1}" -f $el, $desc)
      }
      $reader.Close()
    } catch {
      if ($cts.IsCancellationRequested) {
        Write-Host ("  [{0,6} ms] --- 客户端取消请求（等同点「停止」） ---" -f $sw.ElapsedMilliseconds)
      } else { throw }
    } finally {
      $client.Dispose()
      $cts.Dispose()
    }
    return $events
  }

  Write-Host '流式执行：'
  $events = ApplyStream $pid2 '?stream=1'

  $progress = @($events | Where-Object { $_.ev.type -eq 'progress' })
  $last = ($events | Where-Object { $_.ev.type -eq 'result' } | Select-Object -Last 1).ev
  if ($progress.Count -ne 20) { throw "进度行数 = $($progress.Count)，期望 20" }
  if (-not $last) { throw '没有 result 行' }
  if ($last.body.rows_affected -ne 20000) { throw "rows_affected = $($last.body.rows_affected)，期望 20000" }
  # 进度必须是「陆续到达」：第一条明显早于最后一条，否则就是攒完一次性吐出。
  $span = $progress[-1].at - $progress[0].at
  if ($span -lt 200) { throw "进度在 $span ms 内全部到达，不是流式" }
  Write-Host ("分批执行通过：{0} 条进度，共提交 {1} 行，耗时 {2} ms，进度跨度 {3} ms" -f `
      $progress.Count, $last.body.rows_affected, $last.body.duration_ms, $span)

  # 4) 验证数据确实改了
  $chk = Exec "SELECT COUNT(*) AS n FROM big WHERE v = 'done'"
  $n = $chk.results[0].rows[0][0]
  if ($n -ne 20000) { throw "被改行数 = $n，期望 20000" }
  Write-Output "数据核验通过：v='done' 共 $n 行"

  # 5) 中止：再来一次 2 万行，第一批提交后断开
  Exec "UPDATE big SET v = 'x'" | Out-Null
  $plan2 = Invoke-RestMethod -Uri "$base/connections/$cid/bulk/plan" -Method Post -Headers ($hdr + $json) -Body (@{
      table              = @{ kind = 'table'; name = 'big' }
      set                = @{ v = 'done' }
      confirm_table      = 'big'
      accept_no_rollback = $true
    } | ConvertTo-Json -Depth 6)
  Write-Host '中止测试（curl --max-time 0.6 在传输中途断开，等同界面点「停止」）：'
  $bodyFile = Join-Path $work 'apply.json'
  $appr2 = Approval $plan2.plan.id
  (@{ approval_token = $appr2 } | ConvertTo-Json) | Set-Content -Path $bodyFile -Encoding utf8 -NoNewline
  $curlOut = & curl.exe -sN --max-time 0.6 -X POST `
    -H "X-DBRover-Token: $token" -H 'Content-Type: application/json' `
    --data-binary "@$bodyFile" `
    "$base/changes/$($plan2.plan.id)/apply?stream=1" 2>&1
  Write-Host ("  断开前收到：" + (($curlOut -split "`n" | Where-Object { $_ }) -join ' | '))
  Start-Sleep -Seconds 2
  $chk = Exec "SELECT COUNT(*) AS n FROM big WHERE v = 'done'"
  $n = $chk.results[0].rows[0][0]
  Write-Host "中止后被改行数 = $n"
  if ($n -eq 0) { throw '中止后已提交的批应保留，实际 0 行' }
  if ($n -ge 20000) { throw "中止后不该改完全部 2 万行，实际 $n" }
  Write-Host "中止验证通过：保留 $n 行，剩余 $((20000 - $n)) 行未改"

  # 6) 中止后预案仍有效，可以重新提交把剩下的改完
  $again = $plan2.plan.id
  $events2 = ApplyStream $again '?stream=1'
  $final = Exec "SELECT COUNT(*) AS n FROM big WHERE v = 'done'"
  $n = $final.results[0].rows[0][0]
  Write-Output "重新提交后 v='done' 共 $n 行（幂等补齐）"
  if ($n -ne 20000) { throw "重新提交后应为 20000，实际 $n" }

  Write-Output '全部通过'
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
  Remove-Item -Force -ErrorAction SilentlyContinue $db, "$db-shm", "$db-wal"
}
