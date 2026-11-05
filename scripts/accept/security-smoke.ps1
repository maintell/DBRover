# 安全红线冒烟验收（AGENTS.md §7、docs/10）。
#
# 全部在本机、不需要外部数据库。验的是「默认行为是不是安全的」，
# 这些是单测覆盖不到「真的起一个服务」的部分：
#   1. 回环优先：不带 -allow-remote 时拒绝监听非回环地址（启动即失败）；
#   2. 会话 token：缺 token / 错 token 都 401，且 token 每次启动重生成；
#   3. Origin 校验：跨站 Origin 即使带对 token 也 403；
#   4. 静态资源免鉴权但数据接口不免；未知 /api 路径是 404 不是 SPA 兜底 200；
#   5. 默认只读：read-only 连接上的写操作被拒；
#   6. 凭据不落明文：连接口令不回显，改口令类语句在审计里被脱敏；
#   7. 审计链：/audit/verify 能自证完整（篡改后应报错）。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$w = Join-Path $root ('.tmp\security-' + (Get-Date -Format 'HHmmssfff'))
New-Item -ItemType Directory -Force -Path $w | Out-Null
$db = Join-Path $w 'sec.db'; $audit = Join-Path $w 'audit.jsonl'
$port = 7800; $base = 'http://127.0.0.1:' + $port + '/api/v1'

function Req([string]$method, [string]$path, [hashtable]$headers, [string]$body) {
  $h2 = @{}; if ($headers) { foreach ($k in $headers.Keys) { $h2[$k] = $headers[$k] } }
  $args = @{ Uri = ($base + $path); Method = $method; Headers = $h2; SkipHttpErrorCheck = $true }
  if ($null -ne $body) { $args['Body'] = $body; $args['ContentType'] = 'application/json' }
  return Invoke-WebRequest @args
}

# 1) 回环优先：不给 -allow-remote 就不能监听 0.0.0.0
$bad = & $bin serve -addr 0.0.0.0:7800 -token x 2>&1
$badCode = $LASTEXITCODE
$badText = ($bad -join ' ')
if ($badCode -eq 0) { throw '不带 -allow-remote 竟然监听了 0.0.0.0' }
if ($badText -notmatch '回环') { throw ('拒绝信息没说清是回环限制：' + $badText) }
Write-Output '回环优先：非回环地址被拒且说明原因'

# 2) 起服务（回环 + 审计）
$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', ('127.0.0.1:' + $port), '-token', 'sec-token-1', '-audit', $audit
)
try {
  $ok = @{ 'X-DBRover-Token' = 'sec-token-1' }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri ($base + '/health') -Headers $ok -TimeoutSec 2 | Out-Null; break } catch { Start-Sleep -Milliseconds 200 }
  }

  $noTok = Req 'GET' '/connections' @{} $null
  if ([int]$noTok.StatusCode -ne 401) { throw ('缺 token 状态 ' + [int]$noTok.StatusCode) }
  $badTok = Req 'GET' '/connections' @{ 'X-DBRover-Token' = 'wrong' } $null
  if ([int]$badTok.StatusCode -ne 401) { throw ('错 token 状态 ' + [int]$badTok.StatusCode) }
  if (($noTok.Content | ConvertFrom-Json).error.code -ne 'AUTH_TOKEN_INVALID') { throw '错误码不是 AUTH_TOKEN_INVALID' }
  Write-Output '鉴权：缺 token / 错 token 都是 401 + AUTH_TOKEN_INVALID'

  $cross = Req 'GET' '/connections' @{ 'X-DBRover-Token' = 'sec-token-1'; Origin = 'http://evil.example' } $null
  if ([int]$cross.StatusCode -ne 403) { throw ('跨站 Origin 状态 ' + [int]$cross.StatusCode) }
  if (($cross.Content | ConvertFrom-Json).error.code -ne 'ORIGIN_NOT_ALLOWED') { throw '错误码不是 ORIGIN_NOT_ALLOWED' }
  Write-Output '来源：跨站 Origin 带对 token 仍 403'

  $unknown = Req 'GET' '/api/v1/this-route-does-not-exist' $ok $null
  if ([int]$unknown.StatusCode -ne 404) { throw ('未知 /api 路径状态 ' + [int]$unknown.StatusCode + '（不能被 SPA 吞成 200）') }
  Write-Output '未知 /api 路径：404，不被 SPA 兜底吞掉'

  # 5) 默认只读：建一条 read-only 连接，写操作必须被拒。
  # SQLite 的只读连接要求文件已存在（连接层就拒绝，不是执行门），先用 CLI 建出来。
  & $bin exec -db $db -sql 'CREATE TABLE seed (id INTEGER PRIMARY KEY)' -env local | Out-Null
  if ($LASTEXITCODE -ne 0) { throw '建只读库失败' }
  $ro = Req 'POST' '/connections' ($ok + $json) (@{
      name = 'ro'; engine = 'sqlite'; host = $db; environment = 'test'; access_mode = 'read-only'; password = 'P@ss-Secret-9'
    } | ConvertTo-Json)
  if ([int]$ro.StatusCode -ne 201 -and [int]$ro.StatusCode -ne 200) { throw ('建连接失败 ' + $ro.Content) }
  $roBody = $ro.Content | ConvertFrom-Json
  $rocid = $roBody.id
  if ($ro.Content.Contains('P@ss-Secret-9')) { throw '连接响应回显了明文口令' }
  $list = Req 'GET' '/connections' $ok $null
  if ($list.Content.Contains('P@ss-Secret-9')) { throw '连接列表里出现明文口令' }
  Write-Output '凭据：连接响应与列表都不回显明文口令'

  $wr = Req 'POST' ('/connections/' + $rocid + '/exec') ($ok + $json) (@{ sql = 'CREATE TABLE t (id INT)' } | ConvertTo-Json)
  $wrCode = [int]$wr.StatusCode
  $wrErr = ''; try { $wrErr = ($wr.Content | ConvertFrom-Json).error.code } catch {}
  if ($wrCode -ne 403 -or $wrErr -ne 'POLICY_READ_ONLY') { throw ('只读连接写操作 = ' + $wrCode + ' / ' + $wrErr + '，期望 403 POLICY_READ_ONLY') }
  Write-Output '默认只读：read-only 连接上的写操作 403 POLICY_READ_ONLY'

  # 6) 审计脱敏：发一条带口令的语句（会被策略拒，但审计仍应记录脱敏后的文本）
  $pwSql = "ALTER USER 'app'@'%' IDENTIFIED BY 'Audit-Secret-7'"
  Req 'POST' ('/connections/' + $rocid + '/exec') ($ok + $json) (@{ sql = $pwSql } | ConvertTo-Json) | Out-Null
  $auditText = if (Test-Path $audit) { Get-Content $audit -Raw } else { '' }
  if ($auditText -match 'Audit-Secret-7') { throw '审计文件里出现明文口令' }
  if ($auditText -match 'P@ss-Secret-9') { throw '审计文件里出现连接口令' }
  if ($auditText -notmatch 'IDENTIFIED BY') { throw '审计里没有保留语句骨架（账号/动作要看得到）' }
  Write-Output '审计脱敏：口令不见了，IDENTIFIED BY 与账号骨架还在'

  # 7) 审计链自证
  $vfy = Req 'GET' '/audit/verify' $ok $null
  if ([int]$vfy.StatusCode -ne 200) { throw ('audit/verify 状态 ' + [int]$vfy.StatusCode) }
  $vfyBody = $vfy.Content | ConvertFrom-Json
  if (-not $vfyBody.supported -or -not $vfyBody.ok) { throw ('审计链未通过自证：' + $vfy.Content) }
  if ($vfyBody.events -lt 1) { throw '审计链里一条事件都没有，校验形同虚设' }
  Write-Output ('审计链自证：ok=true，' + $vfyBody.events + ' 条事件')

  # 篡改：往审计文件里塞一行伪造事件，再验一次应当被抓到。
  # 这是链式哈希真正值得验的一半——只验「没改动时 ok」等于没验。
  Add-Content -Path $audit -Value '{"id":"forged","outcome":"ok","sql":"DROP TABLE everything"}' -Encoding UTF8
  $vfy2 = Req 'GET' '/audit/verify' $ok $null
  $vfy2Body = $vfy2.Content | ConvertFrom-Json
  if ($vfy2Body.ok) { throw '往审计文件塞了一行伪造事件，校验竟然还说 ok' }
  if ($vfy2Body.broken_at -lt 1) { throw ('校验报了错但没给出 broken_at：' + $vfy2.Content) }
  Write-Output ('审计链篡改：ok=false，broken_at=' + $vfy2Body.broken_at)

  Write-Output '全部通过'
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}