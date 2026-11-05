# 监控端点在「能力不可用」时的降级行为（docs/09 §2.1、验收 8）。
#
# SQLite 没有性能指标 / 锁等待 / 会话视图，这正是验收「引擎不支持时该怎么表现」
# 的天然样本，不需要外部数据库。它验的是：
#   · 不支持的端点给 400 + 明确理由，不是 500、不是空 200；
#   · 巡检把拿不到数据的项标 unknown（不冒充「没问题」），整体仍是 unknown；
#   · 告警与指标留存不受影响（面板/历史照常可用）。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$w = Join-Path $root ('.tmp\monitor-degrade-' + (Get-Date -Format 'HHmmssfff'))
New-Item -ItemType Directory -Force -Path $w | Out-Null
$db = Join-Path $w 'm.db'; $metrics = Join-Path $w 'metrics.db'
$port = 7799; $base = 'http://127.0.0.1:' + $port + '/api/v1'; $token = 'acc-token-mon'

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', ('127.0.0.1:' + $port), '-token', $token, '-metrics', $metrics
)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri ($base + '/health') -Headers $hdr -TimeoutSec 2 | Out-Null; break } catch { Start-Sleep -Milliseconds 200 }
  }
  $c = Invoke-RestMethod -Uri ($base + '/connections') -Method Post -Headers ($hdr + $json) -Body (@{
      name = 'mon'; engine = 'sqlite'; host = $db; environment = 'test'; access_mode = 'read-write'
    } | ConvertTo-Json)
  $cid = $c.id

  function Get([string]$ep) {
    $r = Invoke-WebRequest -Uri ($base + '/connections/' + $cid + $ep) -Headers $hdr -SkipHttpErrorCheck
    $txt = if ($r.Content -is [byte[]]) { [Text.Encoding]::UTF8.GetString($r.Content) } else { [string]$r.Content }
    return @{ code = [int]$r.StatusCode; body = $txt }
  }

  # 1) 三个「引擎没有这个能力」的端点：明确的 400，而不是 500 / 空 200
  foreach ($ep in @('/monitor/metrics', '/monitor/locks', '/monitor/sessions')) {
    $r = Get $ep
    if ($r.code -ne 400) { throw ($ep + ' 状态 ' + $r.code + '，期望 400') }
    $e = ($r.body | ConvertFrom-Json).error
    if ($e.code -ne 'CAPABILITY_UNSUPPORTED') { throw ($ep + ' 错误码 ' + $e.code) }
    if ($e.message -notmatch 'sqlite') { throw ($ep + ' 理由没点名是哪个引擎：' + $e.message) }
  }
  Write-Output '不支持的监控端点：400 + CAPABILITY_UNSUPPORTED + 点名引擎'

  # 2) 巡检：单项拿不到就标 unknown，整体不能是 ok
  $hc = Get '/monitor/health-check'
  if ($hc.code -ne 200) { throw ('health-check 状态 ' + $hc.code) }
  $rep = $hc.body | ConvertFrom-Json
  if ($rep.level -eq 'ok') { throw '巡检把 unknown 冒充成 ok 了' }
  $allUnknown = $true
  foreach ($it in $rep.items) { if ($it.level -ne 'unknown') { $allUnknown = $false } }
  if (-not $allUnknown) { throw '巡检里有项目不是 unknown（SQLite 上不该有可读数据）' }
  # 四项：连接与缓存 / 锁等待 / 慢查询 / 表体量。「缓存命中率」不是独立一项，
  # 它是嵌在「连接与缓存」分支里的——引擎没有指标时整段消失（docs/09 §5 写「五项」
  # 是目标形态，见本脚本头部说明）。
  if ($rep.items.Count -ne 4) { throw ('巡检有 ' + $rep.items.Count + ' 项，期望 4') }
  Write-Output ('巡检：' + $rep.items.Count + ' 项全为 unknown，整体 ' + $rep.level + '，没有冒充 ok')

  # 3) 告警与留存不受影响
  $al = Get '/monitor/alerts'
  if ($al.code -ne 200) { throw ('alerts 状态 ' + $al.code) }
  if ($al.body -notmatch '只在应用运行时生效') { throw ('alerts 少了「只在运行时生效」的说明：' + $al.body) }
  $hist = Get '/monitor/history?metric=qps&minutes=60'
  if ($hist.code -ne 200) { throw ('history 状态 ' + $hist.code) }
  $h = $hist.body | ConvertFrom-Json
  if (-not $h.available) { throw ('开了 -metrics 但 history 报 available=false：' + $hist.body) }
  Write-Output '告警与指标留存：端点可用，历史 available=true'

  Write-Output '全部通过'
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}