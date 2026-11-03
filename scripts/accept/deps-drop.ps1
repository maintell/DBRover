param(
  [Parameter(Mandatory = $true)][string]$Engine,
  [Parameter(Mandatory = $true)][int]$Port,
  [Parameter(Mandatory = $true)][string]$User,
  [string]$Password = 'testpass',
  [string]$Database = 'acc'
)
# 删除前依赖检查的端到端验收（docs/07 §5.1 / 验收项 8 / ADR-0006）。
#
# 一条链：造父表 + 引用它的视图 + 引用它的外键 → 删表被 409 挡下并列出引用方
# → 拆掉视图与外键 → 出预案（不可逆 + 补偿建表）→ 执行 → 表真的没了。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$httpPort = if ($Engine -eq 'mysql') { 7801 } else { 7802 }
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = "dep-$Engine"

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
    name = "dep-$Engine"; engine = $Engine; host = '192.168.2.88'; port = $Port
    catalog = $Database; user = $User; password = $Password
    environment = 'test'; access_mode = 'read-write'
  }
  if ($Engine -eq 'postgres') { $connBody.schema = 'public' }
  $cid = (Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) `
      -Body ($connBody | ConvertTo-Json)).id

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

  function PlanDropTable {
    try {
      return (Invoke-RestMethod -Uri "$base/connections/$cid/schema/plan" -Method Post -Headers ($hdr + $json) `
        -Body (@{ table = @{ kind = 'table'; name = 'dep_parent' }; action = 'drop_table' } | ConvertTo-Json))
    } catch {
      return ($_.ErrorDetails.Message | ConvertFrom-Json)
    }
  }

  foreach ($q in @(
      'DROP VIEW IF EXISTS dep_view',
      'DROP TABLE IF EXISTS dep_child',
      'DROP TABLE IF EXISTS dep_parent')) {
    try { Exec $q | Out-Null } catch { }
  }
  Exec 'CREATE TABLE dep_parent (id INT NOT NULL PRIMARY KEY)' | Out-Null
  Exec 'CREATE TABLE dep_child (id INT NOT NULL PRIMARY KEY, pid INT)' | Out-Null
  Exec 'CREATE VIEW dep_view AS SELECT id FROM dep_parent' | Out-Null
  Exec 'ALTER TABLE dep_child ADD CONSTRAINT fk_dep FOREIGN KEY (pid) REFERENCES dep_parent (id)' | Out-Null
  Write-Host '  已造好父表 + 视图 + 外键'

  # 1) 有视图与外键 → 409，且两者都要被点名
  $blocked = PlanDropTable
  if (-not $blocked.error -or $blocked.error.code -ne 'DEPENDENCY_BLOCKED') {
    throw "应 409 DEPENDENCY_BLOCKED，实际 $($blocked | ConvertTo-Json -Compress)"
  }
  $msg = $blocked.error.message
  Write-Host "  阻断信息：$msg"
  if ($msg -notmatch 'dep_view') { throw "阻断信息没提到视图: $msg" }
  if ($msg -notmatch 'fk_dep') { throw "阻断信息没提到外键: $msg" }

  # 2) 拆掉两者 → 出预案
  Exec 'DROP VIEW dep_view' | Out-Null
  # 摘外键同一件事两个名字：MySQL 说 DROP FOREIGN KEY，PG 说 DROP CONSTRAINT。
  # 产品里由 dialect.DropForeignKeySQL 负责，脚本这半边自己写就得自己分。
  $dropFK = if ($Engine -eq 'mysql') {
    'ALTER TABLE dep_child DROP FOREIGN KEY fk_dep'
  } else {
    'ALTER TABLE dep_child DROP CONSTRAINT fk_dep'
  }
  Exec $dropFK | Out-Null
  $plan = PlanDropTable
  if ($plan.error) { throw "拆掉依赖后仍失败：$($plan.error.message)" }
  Write-Host "  预案：$($plan.statements[0].sql)"
  if (-not $plan.irreversible) { throw '删表必须标不可逆' }
  if ($plan.notes -notmatch '数据不会回来') { throw "缺少「数据不会回来」的说明：$($plan.notes)" }
  if ($plan.rollback[0].sql -notmatch 'CREATE TABLE') { throw "缺少补偿建表语句：$($plan.rollback[0].sql)" }

  # 3) 执行
  try {
    Invoke-RestMethod -Uri "$base/changes/$($plan.id)/apply" -Method Post -Headers ($hdr + $json) -Body '{}' | Out-Null
  } catch {
    $d = $_.ErrorDetails.Message | ConvertFrom-Json
    if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
    Invoke-RestMethod -Uri "$base/changes/$($plan.id)/apply" -Method Post -Headers ($hdr + $json) `
      -Body (@{ approval_token = $d.error.approval.token } | ConvertTo-Json) | Out-Null
  }
  $tables = Invoke-RestMethod -Uri "$base/connections/$cid/tables" -Headers $hdr
  $names = @($tables.items | ForEach-Object { $_.name })
  if ($names -contains 'dep_parent') { throw '执行后表仍在' }
  Write-Host '  删表通过：表已不存在'

  Exec 'DROP TABLE dep_child' | Out-Null
  Write-Host "$Engine 依赖检查验收通过"
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
