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

  function NewConn([string]$engine, [int]$port, [string]$user, [string]$catalog = 'acc') {
    $b = @{
      name = "cmp-$engine-$catalog"; engine = $engine; host = '192.168.2.88'; port = $port
      catalog = $catalog; user = $user; password = 'testpass'
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

  # 先摘视图再删表：PostgreSQL 不允许删掉被视图引用的表。
  # 上一轮中途失败时留下的视图会挡住这一步，所以清理要在开头就做。
  foreach ($c in @(@{ id = $my.id; ddl = @(
        'DROP VIEW IF EXISTS cmp_v_gone',
        'DROP VIEW IF EXISTS cmp_v_new',
        'DROP VIEW IF EXISTS cmp_v_same',
        'DROP TABLE IF EXISTS cmp_t',
        'CREATE TABLE cmp_t (id BIGINT NOT NULL PRIMARY KEY, v VARCHAR(32))',
        'CREATE INDEX cmp_v ON cmp_t (v)') },
      @{ id = $pg.id; ddl = @(
        'DROP VIEW IF EXISTS cmp_v_gone',
        'DROP VIEW IF EXISTS cmp_v_new',
        'DROP VIEW IF EXISTS cmp_v_same',
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

  # --- 视图定义比对（docs/07 §4.1 / 验收 9）---
  # 视图只比查询体；跨引擎时右侧的 CREATE VIEW 原文是别的方言写的，
  # 不能直接放进升级脚本，所以只给原文并记 need_review。
  Exec $my.id 'CREATE VIEW cmp_v_gone AS SELECT id FROM cmp_t' | Out-Null
  Exec $pg.id 'CREATE VIEW cmp_v_new AS SELECT id FROM cmp_t' | Out-Null
  Write-Host '左侧多一个视图 cmp_v_gone，右侧多一个视图 cmp_v_new'

  $vres = Invoke-RestMethod -Uri "$base/compare" -Method Post -Headers ($hdr + $json) -Body (@{
      left = $my.id; right = $pg.id
    } | ConvertTo-Json)

  $byName = @{}
  foreach ($v in $vres.views) { $byName[$v.name] = $v }
  foreach ($n in @('cmp_v_gone', 'cmp_v_new')) {
    if (-not $byName.ContainsKey($n)) { throw "视图 $n 没有进差异清单（Objects(KindView) 或 DDL 取不到？）" }
  }
  if ($byName['cmp_v_gone'].status -ne 'only_left') { throw "cmp_v_gone 状态 = $($byName['cmp_v_gone'].status)，期望 only_left" }
  if ($byName['cmp_v_new'].status -ne 'only_right') { throw "cmp_v_new 状态 = $($byName['cmp_v_new'].status)，期望 only_right" }
  if (-not $byName['cmp_v_gone'].left) { throw 'cmp_v_gone 没带左侧原文' }
  if (-not $byName['cmp_v_new'].right) { throw 'cmp_v_new 没带右侧原文' }

  $vup = ($vres.upgrade -join "`n")
  $vdown = ($vres.downgrade -join "`n")
  $vreview = ($vres.need_review -join " | ")
  if ($vup -notmatch 'DROP VIEW IF EXISTS') { throw "升级脚本应删掉左侧多出的视图：$vup" }
  if ($vup -match 'CREATE VIEW') { throw "跨引擎时不该把右侧的 CREATE VIEW 原文塞进升级脚本：$vup" }
  if ($vdown -notmatch 'DROP VIEW IF EXISTS') { throw "回退脚本应删掉右侧新增的视图：$vdown" }
  # MySQL 的视图原文是 CREATE ALGORITHM=... DEFINER=... VIEW，不是裸 CREATE VIEW，
  # 所以这里只认视图名——断言「定义被带回来了」这件事本身。
  if ($vdown -notmatch 'cmp_v_gone') { throw "回退脚本应把左侧多出的视图建回来：$vdown" }
  if ($vreview -notmatch '两侧引擎不同') { throw "跨引擎新增视图必须进 need_review：$vreview" }
  Write-Host '视图比对通过：单侧视图分类正确、原文带出、跨引擎 CREATE VIEW 不自动生成'

  # --- 同引擎（MySQL ↔ MySQL，两个库）视图零误报 ---
  # 这一项专门盯 DEFINER 与 ALGORITHM 头部：同一段查询在两个库里由不同账号建，
  # 头部必然不同，但它们不是结构差异。
  Exec $my.id 'CREATE DATABASE IF NOT EXISTS acc2' | Out-Null
  $my2 = NewConn 'mysql' 13306 'root' 'acc2'
  foreach ($pair in @(@{ cid = $my.id }, @{ cid = $my2.id })) {
    foreach ($q in @('DROP VIEW IF EXISTS cmp_v_gone',
        'DROP VIEW IF EXISTS cmp_v_new',
        'DROP VIEW IF EXISTS cmp_v_same',
        'DROP TABLE IF EXISTS cmp_t',
        'CREATE TABLE cmp_t (id BIGINT NOT NULL PRIMARY KEY, v VARCHAR(32))',
        'CREATE INDEX cmp_v ON cmp_t (v)',
        'CREATE VIEW cmp_v_same AS SELECT id FROM cmp_t')) {
      Exec $pair.cid $q | Out-Null
    }
  }
  $sres = Invoke-RestMethod -Uri "$base/compare" -Method Post -Headers ($hdr + $json) -Body (@{
      left = $my.id; right = $my2.id
    } | ConvertTo-Json)
  if (($sres.tables | Measure-Object).Count -ne 0) { throw "同引擎同构表的差异数应为 0，实际 $(($sres.tables | Measure-Object).Count)" }
  if (($sres.views | Measure-Object).Count -ne 0) {
    throw ("同引擎同构视图不该报差异（DEFINER / ALGORITHM 头部被当成了差异？）：" +
      (($sres.views | ForEach-Object { $_.name + '/' + (($_.reason) -join '；') }) -join ' | '))
  }
  Write-Host '同引擎视图零误报通过：DEFINER / ALGORITHM 头部不计入差异'

  foreach ($cid in @($my.id, $pg.id, $my2.id)) {
    try { Exec $cid 'DROP VIEW IF EXISTS cmp_v_gone' | Out-Null } catch { }
    try { Exec $cid 'DROP VIEW IF EXISTS cmp_v_new' | Out-Null } catch { }
    try { Exec $cid 'DROP VIEW IF EXISTS cmp_v_same' | Out-Null } catch { }
    try { Exec $cid 'DROP TABLE IF EXISTS cmp_t' | Out-Null } catch { Write-Host "清理失败：$_" }
  }
  try { Exec $my.id 'DROP DATABASE acc2' | Out-Null } catch { }
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
