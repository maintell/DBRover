param(
  [Parameter(Mandatory = $true)][string]$Engine,
  [Parameter(Mandatory = $true)][int]$Port,
  [Parameter(Mandatory = $true)][string]$User,
  [string]$Password = 'testpass',
  [string]$Database = 'acc'
)
# 加外键的端到端验收（走真实 HTTP 接口，不是直接调 service）。
#
# 验的是整条链：孤儿数据阻断并带样本 → 清数据后出预案 → 执行 → 结构里出现外键
# → 用返回的 undo 预案把它摘掉。最后一条尤其重要：撤销走的是同一个 apply 端点，
# 没有开后门。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$httpPort = if ($Engine -eq 'mysql') { 7797 } else { 7798 }
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = "fk-$Engine"

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
    name = "fk-$Engine"; engine = $Engine; host = '192.168.2.88'; port = $Port
    catalog = $Database; user = $User; password = $Password
    environment = 'test'; access_mode = 'read-write'
  }
  if ($Engine -eq 'postgres') { $connBody.schema = 'public' }
  $conn = Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) `
    -Body ($connBody | ConvertTo-Json)
  $cid = $conn.id

  function Exec([string]$sql) {
    try {
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql } | ConvertTo-Json)
    } catch {
      $d = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql; approval_token = $d.error.approval.token } | ConvertTo-Json)
    }
  }

  function ApplyPlan([string]$planId) {
    try {
      return Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) -Body '{}'
    } catch {
      $d = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return Invoke-RestMethod -Uri "$base/changes/$planId/apply" -Method Post -Headers ($hdr + $json) `
        -Body (@{ approval_token = $d.error.approval.token } | ConvertTo-Json)
    }
  }

  Exec 'DROP TABLE IF EXISTS fk_child' | Out-Null
  Exec 'DROP TABLE IF EXISTS fk_parent' | Out-Null
  Exec 'CREATE TABLE fk_parent (id INT NOT NULL PRIMARY KEY)' | Out-Null
  Exec 'CREATE TABLE fk_child (id INT NOT NULL PRIMARY KEY, pid INT)' | Out-Null
  Exec 'INSERT INTO fk_parent (id) VALUES (1), (2)' | Out-Null
  Exec 'INSERT INTO fk_child (id, pid) VALUES (1, 1), (2, 99)' | Out-Null

  $planBody = @{
    table        = @{ kind = 'table'; name = 'fk_child' }
    action       = 'add_foreign_key'
    foreign_key  = @{
      columns = @('pid'); ref_table = 'fk_parent'; ref_columns = @('id'); on_delete = 'CASCADE'
    }
  } | ConvertTo-Json -Depth 6

  # 1) 有孤儿 → 409 + 样本
  $blocked = $null
  try {
    Invoke-RestMethod -Uri "$base/connections/$cid/schema/plan" -Method Post -Headers ($hdr + $json) `
      -Body $planBody | Out-Null
  } catch {
    $blocked = $_.ErrorDetails.Message | ConvertFrom-Json
  }
  if (-not $blocked -or $blocked.error.code -ne 'FK_PRECHECK_BLOCKED') {
    throw "有孤儿时应 409 FK_PRECHECK_BLOCKED，实际 $($blocked | ConvertTo-Json -Compress)"
  }
  if ($blocked.error.message -notmatch '\(99\)') {
    throw "阻断信息应带孤儿样本 (99)，实际 $($blocked.error.message)"
  }
  Write-Host "  孤儿阻断通过：$($blocked.error.message)"

  # 2) 清掉孤儿 → 出预案
  Exec 'DELETE FROM fk_child WHERE pid = 99' | Out-Null
  $plan = Invoke-RestMethod -Uri "$base/connections/$cid/schema/plan" -Method Post -Headers ($hdr + $json) `
    -Body $planBody
  $sqlText = $plan.statements[0].sql
  Write-Host "  预案：$sqlText"
  if ($sqlText -notmatch 'FOREIGN KEY' -or $sqlText -notmatch 'CASCADE') {
    throw '预案语句不对'
  }
  if ($sqlText -notmatch 'fk_fk_child_pid') {
    throw "应使用生成的确定名字，实际 $sqlText"
  }

  # 3) 执行 → 结构里出现外键
  $out = ApplyPlan $plan.id
  $schema = Invoke-RestMethod -Uri "$base/connections/$cid/tables/fk_child/schema" -Headers $hdr
  $names = @($schema.foreign_keys | ForEach-Object { $_.name })
  if ($names -notcontains 'fk_fk_child_pid') {
    throw "执行后结构里没有该外键：$($names -join ',')"
  }
  Write-Host "  加外键通过：结构里出现 fk_fk_child_pid"

  # 4) 用返回的 undo 预案摘掉（撤销不开后门）
  if (-not $out.undo -or -not $out.undo.id) { throw '加外键后可逆，apply 应返回 undo 预案' }
  $null = ApplyPlan $out.undo.id
  $schema = Invoke-RestMethod -Uri "$base/connections/$cid/tables/fk_child/schema" -Headers $hdr
  $names = @($schema.foreign_keys | ForEach-Object { $_.name })
  if ($names -contains 'fk_fk_child_pid') { throw '撤销后外键仍在' }
  Write-Host "  撤销通过：undo 预案把外键摘掉了"

  Exec 'DROP TABLE fk_child' | Out-Null
  Exec 'DROP TABLE fk_parent' | Out-Null
  Write-Host "$Engine 加外键端到端通过"
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
