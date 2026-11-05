# 导出五格式 + SQL 脚本回放的端到端验收（docs/06 §5、docs/05 §3.1）。
#
# 这段验的是「导出的东西能不能用」，不是「导出函数有没有被调用」：
#   · CSV / NDJSON / Markdown / XLSX 四种格式的形状各自对得上（NULL 与空串、大整数、生成列）；
#   · SQL 导出**真的能回放**：把导出的脚本喂回一个新的 SQLite 库，两行数据与原表一致，
#     生成列由目标端算出来（值正确），而 INSERT 里没有它（写进去引擎会拒绝）。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$w = Join-Path $root ('.tmp\export-formats-' + (Get-Date -Format 'HHmmssfff'))
New-Item -ItemType Directory -Force -Path $w | Out-Null
$src = Join-Path $w 'src.db'; $dst = Join-Path $w 'dst.db'
$port = 7796; $base = 'http://127.0.0.1:' + $port + '/api/v1'; $token = 'acc-token-exp'

$srv = Start-Process -FilePath $bin -PassThru -WindowStyle Hidden -ArgumentList @('serve', '-addr', ('127.0.0.1:' + $port), '-token', $token)
try {
  $hdr = @{ 'X-DBRover-Token' = $token }
  $json = @{ 'Content-Type' = 'application/json' }
  for ($i = 0; $i -lt 60; $i++) {
    try { Invoke-RestMethod -Uri ($base + '/health') -Headers $hdr -TimeoutSec 2 | Out-Null; break } catch { Start-Sleep -Milliseconds 250 }
  }

  $conn = Invoke-RestMethod -Uri ($base + '/connections') -Method Post -Headers ($hdr + $json) -Body (@{
      name = 'exp'; engine = 'sqlite'; host = $src; environment = 'test'; access_mode = 'read-write'
    } | ConvertTo-Json)
  $cid = $conn.id

  # exec 带审批：写语句会 428，拿 token 重放一次（与界面同一条路）
  function Exec([string]$sql, [string]$onCid) {
    # onCid 可指定目标连接：SQL 回放后的校验必须查目标库，
    # 写死源库的话「回放成功」是假验证（源库本来就有那两行）。
    if (-not $onCid) { $onCid = $cid }
    $body = @{ sql = $sql } | ConvertTo-Json
    $r = Invoke-WebRequest -Uri ($base + '/connections/' + $onCid + '/exec') -Method Post -Headers ($hdr + $json) -Body $body -SkipHttpErrorCheck
    if ([int]$r.StatusCode -eq 428) {
      $tok = ($r.Content | ConvertFrom-Json).error.approval.token
      $r = Invoke-WebRequest -Uri ($base + '/connections/' + $onCid + '/exec') -Method Post -Headers ($hdr + $json) `
        -Body (@{ sql = $sql; approval_token = $tok } | ConvertTo-Json) -SkipHttpErrorCheck
    }
    if ([int]$r.StatusCode -ne 200) { throw ('exec 失败（' + $sql + '）：' + $r.Content) }
    return $r
  }

  Exec 'CREATE TABLE ex (id INTEGER PRIMARY KEY, txt TEXT, empty TEXT, nul TEXT, big INTEGER, gen INTEGER GENERATED ALWAYS AS (id * 2) STORED)' | Out-Null
  Exec @("INSERT INTO ex (id, txt, empty, nul, big) VALUES (1, '世界', '', NULL, 9007199254740993)")[0] | Out-Null
  # 值里带逗号和双引号：CSV / Markdown 的转义都得过一遍
  $dq = [char]34
  Exec ("INSERT INTO ex (id, txt, empty, nul, big) VALUES (2, 'a,b" + $dq + "c', '', NULL, 42)") | Out-Null

  function Export([string]$fmt) {
    return Invoke-WebRequest -Uri ($base + '/connections/' + $cid + '/export?table=ex&format=' + $fmt) -Headers $hdr -SkipHttpErrorCheck
  }

  $csv = Export 'csv'
  if ([int]$csv.StatusCode -ne 200) { throw ('csv 状态 ' + [int]$csv.StatusCode) }
  $csvText = $csv.Content
  if ($csvText -notmatch '世界') { throw 'CSV 里没有中文值' }
  if ($csvText -notmatch '9007199254740993') { throw 'CSV 里的大整数不是原值' }
  Write-Output 'CSV：中文与大整数保真'

  $md = Export 'markdown'
  $mdText = $md.Content
  if ($mdText -notmatch '\| id \|') { throw ('markdown 缺表头：' + $mdText) }
  if ($mdText -notmatch 'NULL') { throw ('markdown 没把 NULL 标出来：' + $mdText) }
  if ($mdText -notmatch "''") { throw ('markdown 没把空串标出来：' + $mdText) }
  Write-Output 'Markdown：表头 + NULL 与空串区分'

  $nd = Export 'ndjson'
  # application/x-ndjson 在 PowerShell 里回来的是 byte[]（不是 string），
  # 直接 [string] 会得到「123 34 105 …」这种字节列表——必须显式解码。
  $ndText = if ($nd.Content -is [byte[]]) { [Text.Encoding]::UTF8.GetString($nd.Content) } else { [string]$nd.Content }
  $lines = @($ndText.TrimEnd() -split '\r?\n' | Where-Object { $_.Trim() -ne '' })
  if ($lines.Count -ne 2) { throw ('ndjson 行数 ' + $lines.Count + '，期望 2') }
  $first = $lines[0] | ConvertFrom-Json
  if ($first.big -isnot [string]) { throw ('ndjson 的大整数应是字符串，实际 ' + $first.big.GetType().Name) }
  if ($first.big -ne '9007199254740993') { throw ('ndjson 大整数 = ' + $first.big) }
  if ($null -ne $first.nul) { throw 'ndjson 里 nul 应是 null' }
  if ($first.empty -ne '') { throw ('ndjson 里 empty 应是空串，实际 ' + $first.empty) }
  # 整数在 NDJSON 里一律是字符串（BigInt 保真：JSON 数字超过 2^53 会丢精度），
  # 所以这里按字符串比较，而不是指望它是数字。
  if ([string]$first.gen -ne '2') { throw ('ndjson 里生成列应是算出来的 2，实际 ' + $first.gen) }
  Write-Output 'NDJSON：大整数为字符串、null 与 "" 区分、生成列带值'

  $sqlRes = Export 'sql'
  # 与 ndjson 一样，非文本的 Content-Type 会返回 byte[]，必须显式解码。
  $sqlText = if ($sqlRes.Content -is [byte[]]) { [Text.Encoding]::UTF8.GetString($sqlRes.Content) } else { [string]$sqlRes.Content }
  if ($sqlText -notmatch 'CREATE TABLE') { throw 'SQL 导出里没有建表语句' }
  foreach ($line in ($sqlText -split '\r?\n')) {
    if ($line.StartsWith('INSERT INTO') -and $line.Contains('"gen"')) {
      throw ('SQL 导出的 INSERT 里出现了生成列：' + $line)
    }
  }
  if ($sqlText -notmatch '生成列 gen 不写入') { throw 'SQL 导出头没有说明生成列不写入' }
  Write-Output 'SQL：有建表、INSERT 跳过生成列、头里有说明'

  $xlsx = Export 'xlsx'
  if ([int]$xlsx.StatusCode -ne 200) { throw ('xlsx 状态 ' + [int]$xlsx.StatusCode) }
  if ($xlsx.Headers['Content-Type'] -notmatch 'spreadsheetml') { throw ('xlsx Content-Type = ' + $xlsx.Headers['Content-Type']) }
  $bytes = $xlsx.RawContentStream.ToArray()
  if ($bytes.Length -lt 2 -or $bytes[0] -ne 0x50 -or $bytes[1] -ne 0x4B) { throw 'xlsx 不是 zip 容器（缺 PK 头）' }
  Write-Output ('XLSX：zip 容器 ' + $bytes.Length + ' 字节')

  # SQL 回放：喂回一个新库，数据与生成列都要对得上
  $conn2 = Invoke-RestMethod -Uri ($base + '/connections') -Method Post -Headers ($hdr + $json) -Body (@{
      name = 'exp2'; engine = 'sqlite'; host = $dst; environment = 'test'; access_mode = 'read-write'
    } | ConvertTo-Json)
  $cid2 = $conn2.id
  $body = @{ sql = $sqlText } | ConvertTo-Json -Depth 4
  $r = Invoke-WebRequest -Uri ($base + '/connections/' + $cid2 + '/exec') -Method Post -Headers ($hdr + $json) -Body $body -SkipHttpErrorCheck
  if ([int]$r.StatusCode -eq 428) {
    $tok = ($r.Content | ConvertFrom-Json).error.approval.token
    $r = Invoke-WebRequest -Uri ($base + '/connections/' + $cid2 + '/exec') -Method Post -Headers ($hdr + $json) `
      -Body (@{ sql = $sqlText; approval_token = $tok } | ConvertTo-Json -Depth 4) -SkipHttpErrorCheck
  }
  if ([int]$r.StatusCode -ne 200) { throw ('回放失败：' + $r.Content) }

  $check = Exec 'SELECT id, gen FROM ex ORDER BY id' $cid2
  $rows = ($check.Content | ConvertFrom-Json).results[0].rows
  if ($rows.Count -ne 2) { throw ('回放后行数 ' + $rows.Count + '，期望 2') }
  if ($rows[0][1] -ne 2 -or $rows[1][1] -ne 4) { throw ('回放后生成列 = ' + $rows[0][1] + ',' + $rows[1][1] + '，期望 2,4') }
  Write-Output 'SQL 回放：2 行数据回来，生成列由目标端算出 2 / 4'

  Write-Output '全部通过'
}
finally {
  if ($srv -and -not $srv.HasExited) { Stop-Process -Id $srv.Id -Force }
}