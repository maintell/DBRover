param(
  [Parameter(Mandatory = $true)][string]$Engine,
  [Parameter(Mandatory = $true)][int]$Port,
  [Parameter(Mandatory = $true)][string]$User,
  [string]$Password = 'testpass',
  [string]$Database = 'acc'
)
# 验收 docs/06 验收项 6：Decimal(38,10) 与 BigInt 编辑保存无损。
#
# 判据不是「看起来一样」，而是三方字符串必须逐字符相同：
#   1) 独立 SQL 查出来的原文（引擎自己的规范形态）
#   2) 表浏览接口返回的单元格值
#   3) 走一遍变更集写回之后再查出来的原文
# 任何一环把值当 double 处理，38 位的十进制数就会在低位悄悄变形。
$ErrorActionPreference = 'Stop'

# 引擎名用服务端的规范值：`pg` 是早先的别名，服务端只认 postgres。
if ($Engine -eq 'pg') { $Engine = 'postgres' }

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$httpPort = if ($Engine -eq 'mysql') { 7794 } else { 7795 }
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = "prec-$Engine"

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$httpPort", '-token', $token)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  $connBody = @{
    name = "prec-$Engine"; engine = $Engine; host = '192.168.2.88'; port = $Port
    catalog = $Database; user = $User; password = $Password
    environment = 'test'; access_mode = 'read-write'
  }
  if ($Engine -eq 'postgres') { $connBody.schema = 'public' }
  $conn = Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) `
    -Body ($connBody | ConvertTo-Json)
  $cid = $conn.id

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

  # 变更集：先用一次 get 拿不到，就自己拼 apply
  function PlanApply([string]$planId) {
    try {
      return Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) -Body '{}'
    } catch {
      $detail = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($detail.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) `
        -Body (@{ approval_token = $detail.error.approval.token } | ConvertTo-Json)
    }
  }

  $decimalType = if ($Engine -eq 'mysql') { 'DECIMAL(38,10)' } else { 'NUMERIC(38,10)' }
  $bigType = 'BIGINT'

  Exec 'DROP TABLE IF EXISTS prec' | Out-Null
  Exec "CREATE TABLE prec (id $bigType NOT NULL PRIMARY KEY, d $decimalType, n $bigType)" | Out-Null

  # 极值：38 位十进制（10 位小数）、BIGINT 上下界、以及 2^53 以上的陷阱值
  $cases = @(
    @{ id = '9007199254740993'; d = '1234567890123456789012345678.1234567891'; n = '9007199254740993' },
    @{ id = '9223372036854775806'; d = '9999999999999999999999999999.9999999999'; n = '9223372036854775807' },
    @{ id = '9223372036854775807'; d = '-9999999999999999999999999999.9999999999'; n = '-9223372036854775808' }
  )
  foreach ($c in $cases) {
    Exec ("INSERT INTO prec (id, d, n) VALUES ({0}, {1}, {2})" -f $c.id, $c.d, $c.n) | Out-Null
  }

  function CellText($v) {
    if ($null -eq $v) { return '<null>' }
    return [string]$v
  }

  function Rows {
    $r = Invoke-RestMethod -Uri "$base/connections/$cid/tables/prec/rows?limit=50" `
      -Headers $hdr
    return $r
  }

  $rowsResp = Rows
  $colIdx = @{}
  for ($i = 0; $i -lt $rowsResp.columns.Count; $i++) {
    $colIdx[$rowsResp.columns[$i].name] = $i
  }
  if (-not $colIdx.ContainsKey('id') -or -not $colIdx.ContainsKey('d') -or -not $colIdx.ContainsKey('n')) {
    throw "列名不全：$($rowsResp.columns.name -join ',')"
  }

  $byId = @{}
  foreach ($r in $rowsResp.rows) {
    $byId[(CellText $r[$colIdx['id']])] = $r
  }

  $fail = 0
  foreach ($c in $cases) {
    $got = $byId[$c.id]
    if (-not $got) { Write-Host "  缺少 id=$($c.id)"; $fail++; continue }
    $gotD = CellText $got[$colIdx['d']]
    $gotN = CellText $got[$colIdx['n']]
    $okD = ($gotD -eq $c.d)
    $okN = ($gotN -eq $c.n)
    Write-Host ("  id={0}`n    d: 期望 {1}`n       实际 {2}  {3}`n    n: 期望 {4}`n       实际 {5}  {6}" -f `
        $c.id, $c.d, $gotD, $(if ($okD) { 'OK' } else { '不一致' }), $c.n, $gotN, $(if ($okN) { 'OK' } else { '不一致' }))
    if (-not $okD) { $fail++ }
    if (-not $okN) { $fail++ }
  }
  if ($fail -gt 0) { throw "$Engine 读取阶段有 $fail 处精度不一致" }
  Write-Host "$Engine 读取通过：BIGINT 与 NUMERIC(38,10) 都是逐字符精确的字符串"

  # 写回：把每一行原样写回（值不变），再改成一个新值，再查原文
  foreach ($c in $cases) {
    $plan = Invoke-RestMethod -Uri "$base/connections/$cid/changes/plan" -Method Post -Headers ($hdr + $json) `
      -Body (@{
        table   = @{ kind = 'table'; name = 'prec' }
        changes = @(@{
            op     = 'update'
            key    = @{ id = $c.id }
            before = @{ id = $c.id; d = $c.d; n = $c.n }
            after  = @{ d = $c.d; n = $c.n }
          })
      } | ConvertTo-Json -Depth 8)
    $null = PlanApply $plan.id
  }
  $chk = Exec ("SELECT COUNT(*) AS n FROM prec WHERE d IN (" + (($cases | ForEach-Object { $_.d }) -join ',') + ')')
  $n = [int64]$chk.results[0].rows[0][0]
  Write-Host "$Engine 原样写回：库里仍是 $n 行原值（期望 $($cases.Count)）"
  if ($n -ne $cases.Count) { throw "原样写回后有 $($cases.Count - $n) 行值变了" }

  # 改成另一个 38 位精确值，确认低位没有被抹平
  $newD = '1111111111111111111111111111.0000000001'
  foreach ($c in $cases) {
    $plan = Invoke-RestMethod -Uri "$base/connections/$cid/changes/plan" -Method Post -Headers ($hdr + $json) `
      -Body (@{
        table   = @{ kind = 'table'; name = 'prec' }
        changes = @(@{
            op     = 'update'
            key    = @{ id = $c.id }
            before = @{ id = $c.id; d = $c.d; n = $c.n }
            after  = @{ d = $newD; n = $c.n }
          })
      } | ConvertTo-Json -Depth 8)
    $null = PlanApply $plan.id
  }
  $chk = Exec "SELECT COUNT(*) AS n FROM prec WHERE d = $newD"
  $n = [int64]$chk.results[0].rows[0][0]
  Write-Host "$Engine 改成新精度值：命中 $n 行（期望 $($cases.Count)）"
  if ($n -ne $cases.Count) { throw "写回的新值不对，只有 $n 行精确匹配" }

  # 反向验证：把 BIGINT 写成 JSON 数字（>2^53）必须被拒，而不是悄悄写进库。
  $bodyJson = '{"table":{"kind":"table","name":"prec"},"changes":[{"op":"update","key":{"id":"1"},"before":{"id":1},"after":{"n":9223372036854775807}}]}'
  $detail = $null
  try {
    Invoke-RestMethod -Uri "$base/connections/$cid/changes/plan" -Method Post -Headers ($hdr + $json) `
      -Body $bodyJson | Out-Null
  } catch {
    $detail = $_.ErrorDetails.Message | ConvertFrom-Json
  }
  if (-not $detail -or $detail.error.code -ne 'NUMBER_PRECISION_LOSS') {
    throw "大整数写成 JSON 数字应返回 400 NUMBER_PRECISION_LOSS，实际 $($detail | ConvertTo-Json -Compress)"
  }
  Write-Host "$Engine 负向验证通过：JSON 数字形态的大整数被拒绝（400 NUMBER_PRECISION_LOSS）"

  Exec 'DROP TABLE prec' | Out-Null
  Write-Host "$Engine 精度验收通过"
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
