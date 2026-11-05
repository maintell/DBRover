# 「遇错继续」与流式查询入口的端到端验收（docs/05 §2.1、docs/12 的错误码）。
#
# 三件事，都是单测证明不了「接口那层也对」的：
#   1. 默认遇错即停：第一条成功的结果要带回来，第二条的错要带回来，第三条不跑；
#   2. stop_on_error=false：三条都跑，失败那条带自己的错，成功那条带自己的行；
#   3. /query 只接单条只读语句：多语句一律 400 EXEC_INVALID（不是语法错，是请求形态错）。
# 走真实 HTTP 服务 + 真实 SQLite，不需要外部数据库。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$work = Join-Path $root '.tmp\stop-on-error'
$db = Join-Path $work 'soe.db'
$port = 7795
$base = "http://127.0.0.1:$port/api/v1"
$token = 'acc-token-soe'

New-Item -ItemType Directory -Force -Path $work | Out-Null
Remove-Item -Force -ErrorAction SilentlyContinue $db

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$port", '-token', $token
)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }

  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  $conn = Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) -Body (@{
      name = 'stop-on-error'; engine = 'sqlite'; host = $db
      environment = 'test'; access_mode = 'read-write'
    } | ConvertTo-Json)
  $cid = $conn.id

  # post 返回 { status, body }：错误体也要能读到，所以不让 Invoke-WebRequest 抛。
  function Post([string]$path, [hashtable]$body) {
    $r = Invoke-WebRequest -Uri ("$base" + $path) -Method Post -Headers ($hdr + $json) `
      -Body ($body | ConvertTo-Json -Depth 8) -SkipHttpErrorCheck
    $parsed = $null
    if ($r.Content) { $parsed = $r.Content | ConvertFrom-Json }
    return @{ status = [int]$r.StatusCode; body = $parsed }
  }

  $script = @"
SELECT 1 AS a;
SELECT * FROM 没有这张表;
SELECT 3 AS c
"@

  # 1) 默认：遇错即停
  $r1 = Post "/connections/$cid/exec" @{ sql = $script }
  if ($r1.status -ne 400) { throw "默认遇错即停应返回 400，实际 $($r1.status)" }
  if ($r1.body.error.code -ne 'SQL_OBJECT_NOT_FOUND') {
    throw "错误码应是 SQL_OBJECT_NOT_FOUND，实际 $($r1.body.error.code)"
  }
  $res1 = @($r1.body.error.results)
  if ($res1.Count -ne 2) { throw "默认模式应只跑前两条（结果数 2），实际 $($res1.Count)" }
  if ($res1[0].rows.Count -ne 1 -or $res1[0].rows[0][0] -ne 1) {
    throw "第一条的结果必须带回来（否则用户以为一条都没跑）"
  }
  if (-not $res1[1].err) { throw "第二条应带执行期错误" }
  Write-Output "默认遇错即停：2 条结果，第 2 条失败，第 3 条未执行"

  # 2) 显式 stop_on_error=false：继续执行
  $r2 = Post "/connections/$cid/exec" @{ sql = $script; stop_on_error = $false }
  if ($r2.status -ne 400) { throw "继续执行时仍应返回 400（失败不会因为继续而消失），实际 $($r2.status)" }
  $res2 = @($r2.body.error.results)
  if ($res2.Count -ne 3) { throw "继续执行应跑完三条（结果数 3），实际 $($res2.Count)" }
  if ($res2[2].err) { throw "第三条应成功，实际 $($res2[2].err)" }
  if ($res2[2].rows[0][0] -ne 3) { throw "第三条应带回 3，实际 $($res2[2].rows[0][0])" }
  Write-Output "继续执行：3 条结果，第 3 条成功并带行数据"

  # 3) /query 只接单条只读语句
  $r3 = Post "/connections/$cid/query" @{ sql = $script }
  if ($r3.status -ne 400) { throw "/query 收到多语句应 400，实际 $($r3.status)" }
  if ($r3.body.error.code -ne 'EXEC_INVALID') {
    throw "/query 的错误码应是 EXEC_INVALID，实际 $($r3.body.error.code)"
  }
  Write-Output "流式查询入口：多语句被 EXEC_INVALID 拒掉"

  # 4) 单条只读语句要真的能开游标（否则上一条断言可能只是「永远拒绝」）
  $r4 = Post "/connections/$cid/query" @{ sql = 'SELECT 1 AS a' }
  if ($r4.status -ne 200) { throw "单条只读语句应 200，实际 $($r4.status)" }
  if (-not $r4.body.cursor_id) { throw "应返回 cursor_id" }
  $page = Invoke-WebRequest -Uri "$base/results/$($r4.body.cursor_id)/page?limit=10" `
    -Headers $hdr -SkipHttpErrorCheck
  if ([int]$page.StatusCode -ne 200) { throw "取页应 200，实际 $([int]$page.StatusCode)" }
  Invoke-WebRequest -Uri "$base/results/$($r4.body.cursor_id)" -Method Delete -Headers $hdr -SkipHttpErrorCheck | Out-Null
  Write-Output "流式查询入口：单条只读语句可开游标并取页"

  Write-Output '全部通过'
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
  Get-Process dbrover -ErrorAction SilentlyContinue | Where-Object { $_.Id -eq $srv.Id } | Stop-Process -Force
}
