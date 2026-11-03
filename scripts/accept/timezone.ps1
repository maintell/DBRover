param(
  [Parameter(Mandatory = $true)][string]$Engine,
  [Parameter(Mandatory = $true)][int]$Port,
  [Parameter(Mandatory = $true)][string]$User,
  [string]$Password = 'testpass',
  [string]$Database = 'acc'
)
# 展示时区验收（docs/06 验收项 11）：timestamptz 按配置时区显示，写回仍是 UTC。
#
# 判据是「同一个瞬间的两种呈现」：
#   同一条数据，用 display_timezone=Asia/Shanghai 的连接读出来是 12:00+08:00，
#   用默认（UTC）的连接读出来是 04:00Z。两个字符串必须表示同一时刻，
#   而且库里存的没被改动。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$httpPort = if ($Engine -eq 'mysql') { 7799 } else { 7800 }
$base = "http://127.0.0.1:$httpPort/api/v1"
$token = "tz-$Engine"

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @(
  'serve', '-addr', "127.0.0.1:$httpPort", '-token', $token)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri "$base/health" -Headers $hdr -TimeoutSec 2 | Out-Null; break }
    catch { Start-Sleep -Milliseconds 250 }
  }

  function NewConn([string]$tz) {
    $b = @{
      name = "tz-$Engine-$(if ($tz) { 'sh' } else { 'utc' })"; engine = $Engine
      host = '192.168.2.88'; port = $Port; catalog = $Database; user = $User; password = $Password
      environment = 'test'; access_mode = 'read-write'
    }
    if ($Engine -eq 'postgres') { $b.schema = 'public' }
    if ($tz) { $b.display_timezone = $tz }
    return (Invoke-RestMethod -Uri "$base/connections" -Method Post -Headers ($hdr + $json) -Body ($b | ConvertTo-Json))
  }

  function Exec([string]$cid, [string]$sql) {
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

  $sh = NewConn 'Asia/Shanghai'
  $utc = NewConn ''
  Write-Host "  已建两个连接：Asia/Shanghai 与默认（UTC）"

  $colType = if ($Engine -eq 'mysql') { 'TIMESTAMP' } else { 'timestamptz' }
  $lit = if ($Engine -eq 'mysql') { "'2026-11-03 04:00:00'" } else { "'2026-11-03 04:00:00+00'" }

  Exec $sh.id 'DROP TABLE IF EXISTS tz_demo' | Out-Null
  Exec $sh.id "CREATE TABLE tz_demo (id INT NOT NULL PRIMARY KEY, at $colType)" | Out-Null
  Exec $sh.id "INSERT INTO tz_demo (id, at) VALUES (1, $lit)" | Out-Null

  # 用 curl 取原文：Invoke-RestMethod 会把 RFC3339 字符串自动转成 DateTime，
  # 恰好把「带什么偏移」这个最关键的证据吃掉。
  function CellTime([string]$cid) {
    $raw = & curl.exe -s -H "X-DBRover-Token: $token" "$base/connections/$cid/tables/tz_demo/rows?limit=5"
    if ($raw -notmatch '"(\d{4}-\d{2}-\d{2}T[^"]+)"') {
      throw "响应里没有 RFC3339 时间：$raw"
    }
    return $Matches[1]
  }

  $shText = CellTime $sh.id
  $utcText = CellTime $utc.id
  Write-Host "  Asia/Shanghai 连接读到：$shText"
  Write-Host "  默认（UTC）连接读到：$utcText"

  if ($shText -notmatch '12:00:00\+08:00') {
    throw "按 Asia/Shanghai 展示应为 12:00:00+08:00，实际 $shText"
  }
  if ($utcText -notmatch '04:00:00(Z|\+00:00)') {
    throw "按 UTC 展示应为 04:00:00Z，实际 $utcText"
  }

  # 两个字符串必须表示同一瞬间：把两边都解析成 DateTimeOffset 再比。
  $a = [datetimeoffset]::Parse($shText)
  $b = [datetimeoffset]::Parse($utcText)
  if ($a.UtcDateTime -ne $b.UtcDateTime) {
    throw "两种展示不是同一时刻：$($a.UtcDateTime) vs $($b.UtcDateTime)"
  }
  Write-Host "  两种展示指向同一时刻：$($a.UtcDateTime.ToString('u'))"

  # 写回：在 Asia/Shanghai 连接上把时间改成同一瞬间的另一种写法（带偏移），
  # 再用 UTC 连接读回，必须还是同一时刻（说明存储是 UTC，展示才换算）。
  $plan = Invoke-RestMethod -Uri "$base/connections/$($sh.id)/changes/plan" -Method Post -Headers ($hdr + $json) `
    -Body (@{
      table   = @{ kind = 'table'; name = 'tz_demo' }
      changes = @(@{
          op     = 'update'
          key    = @{ id = 1 }
          before = @{ id = 1; at = $shText }
          after  = @{ at = '2026-11-03T05:30:00+08:00' }
        })
    } | ConvertTo-Json -Depth 8)
  $applyBody = '{}'
  try {
    Invoke-RestMethod -Uri "$base/changes/$($plan.id)/apply" -Method Post -Headers ($hdr + $json) -Body $applyBody | Out-Null
  } catch {
    $d = $_.ErrorDetails.Message | ConvertFrom-Json
    if ($d.error.code -ne 'POLICY_APPROVAL_REQUIRED') { throw }
    Invoke-RestMethod -Uri "$base/changes/$($plan.id)/apply" -Method Post -Headers ($hdr + $json) `
      -Body (@{ approval_token = $d.error.approval.token } | ConvertTo-Json) | Out-Null
  }

  $afterSh = CellTime $sh.id
  $afterUtc = CellTime $utc.id
  Write-Host "  写回后 Asia/Shanghai 连接读到：$afterSh"
  Write-Host "  写回后默认（UTC）连接读到：$afterUtc"

  # 05:30+08:00 就是 21:30Z（前一天）。用 UTC 连接读必须看到 21:30Z。
  if ($afterUtc -notmatch '21:30:00(Z|\+00:00)') {
    throw "写回后 UTC 应该是 21:30:00Z（05:30+08:00），实际 $afterUtc"
  }
  if ($afterSh -notmatch '05:30:00\+08:00') {
    throw "写回后按 Asia/Shanghai 应读回 05:30:00+08:00，实际 $afterSh"
  }
  Write-Host "  写回为 UTC 通过：带偏移的值按瞬间存储，展示时才换算"

  Exec $sh.id 'DROP TABLE tz_demo' | Out-Null
  Write-Host "$Engine 展示时区验收通过"
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}
