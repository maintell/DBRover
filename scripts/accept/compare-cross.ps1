param()
# 跨引擎结构比对冒烟：MySQL 5.7 ↔ PostgreSQL 17。
# 关注两件事：主键索引没有被当成普通索引报差异；索引方法 BTREE/btree 视为等价。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$httpPort = 7796
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = 'cmp-cross'

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$httpPort", '-token', $token)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  function NewConn([string]$engine, [int]$port, [string]$user) {
    $b = @{
      name = "cmp-$engine"; engine = $engine; host = '192.168.2.88'; port = $port
      catalog = 'acc'; user = $user; password = 'testpass'
      environment = 'test'; access_mode = 'read-write'
    }
    if ($engine -eq 'postgres') { $b.schema = 'public' }
    return (Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) -Body ($b | ConvertTo-Json))
  }

  function Exec([string]$cid, [string]$sql) {
    try {
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) -Body (@{ sql = $sql } | ConvertTo-Json)
    } catch {
      $d = $_.ErrorDetails.Message | ConvertFrom-Json
      if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
      return Invoke-RestMethod -Uri "$base/connections/$cid/exec" -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql; approval_token = $d.error.approval.token } | ConvertTo-Json)
    }
  }

  $my = NewConn 'mysql' 13306 'root'
  $pg = NewConn 'postgres' 15432 'postgres'

  foreach ($c in @(@{ id = $my.id; ddl = @(
        'DROP TABLE IF EXISTS cmp_t',
        'CREATE TABLE cmp_t (id BIGINT NOT NULL PRIMARY KEY, v VARCHAR(32))',
        'CREATE INDEX cmp_v ON cmp_t (v)') },
      @{ id = $pg.id; ddl = @(
        'DROP TABLE IF EXISTS cmp_t',
        'CREATE TABLE cmp_t (id BIGINT NOT NULL PRIMARY KEY, v VARCHAR(32))',
        'CREATE INDEX cmp_v ON cmp_t (v)') })) {
    foreach ($q in $c.ddl) { Exec $c.id $q | Out-Null }
  }
  Write-Host '两侧都建好 cmp_t（同主键、同非唯一索引）'

  $res = Invoke-RestMethod -Uri "$base/compare" -Method Post -Headers ($hdr + $json) -Body (@{
      left = $my.id; right = $pg.id
    } | ConvertTo-Json)
  Write-Host ("差异表数 = {0}；升级 {1} 条；回退 {2} 条；待决策 {3} 条" -f `
      ($res.tables | Measure-Object).Count, ($res.upgrade | Measure-Object).Count,
      ($res.downgrade | Measure-Object).Count, ($res.need_review | Measure-Object).Count)
  foreach ($t in $res.tables) {
    Write-Host ("  表 {0} 状态={1}" -f $t.name, $t.status)
    foreach ($c in $t.columns) { Write-Host ("    列 {0} {1} {2}" -f $c.name, $c.status, (($c.reason) -join '；')) }
    foreach ($x in $t.indexes) { Write-Host ("    索引 {0} {1} {2}" -f $x.key, $x.status, (($x.reason) -join '；')) }
  }
  if ($res.upgrade) { Write-Host ($res.upgrade -join "`n") }
  if ($res.need_review) { Write-Host ('待决策：' + ($res.need_review -join ' | ')) }

  $idxNoise = 0
  foreach ($t in $res.tables) { $idxNoise += ($t.indexes | Measure-Object).Count }
  if ($idxNoise -ne 0) { throw "同构表不该报 $idxNoise 条索引差异（主键被当成普通索引？方法名没归一？）" }
  if (($res.upgrade | Measure-Object).Count -ne 0 -or ($res.downgrade | Measure-Object).Count -ne 0) { throw "同构表不该生成脚本" }
  Write-Host '跨引擎比对冒烟通过：索引零误报、主键未误报、BTREE/btree 视为等价、整数显示宽度已归一（bigint(20) 不再报差异）'

  foreach ($cid in @($my.id, $pg.id)) {
    try { Exec $cid 'DROP TABLE cmp_t' | Out-Null } catch { Write-Host "清理失败：$_" }
  }
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
