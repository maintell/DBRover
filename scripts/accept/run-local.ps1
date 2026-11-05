# 一次跑完所有「不需要真机」的验收脚本。
#
# 为什么要这个入口：README 里写着哪几条不需要外部数据库，但每次都要人肉逐个跑
# 再自己核对退出码。这里把「跑没跑过」变成一条命令的结果——失败时把最后 15 行
# 输出打出来，不用回头翻日志。真机脚本不在这里（它们需要容器，见 README 上方）。
$ErrorActionPreference = 'Stop'

$scripts = @(
  'bulk-20k-sqlite.ps1',
  'stop-on-error.ps1',
  'backup-schema-only.ps1',
  'export-formats.ps1',
  'monitor-degrade.ps1'
)

$fail = 0
foreach ($s in $scripts) {
  $path = Join-Path $PSScriptRoot $s
  if (-not (Test-Path $path)) {
    $fail++
    Write-Output ('FAIL  ' + $s + '（文件不存在）')
    continue
  }
  $sw = [Diagnostics.Stopwatch]::StartNew()
  $out = & pwsh -NoProfile -File $path 2>&1
  $code = $LASTEXITCODE
  $sw.Stop()
  $secs = [Math]::Round($sw.Elapsed.TotalSeconds, 1)
  if ($code -eq 0) {
    Write-Output ('PASS  ' + $s.PadRight(28) + $secs + 's')
  } else {
    $fail++
    Write-Output ('FAIL  ' + $s.PadRight(28) + $secs + 's')
    $out | Select-Object -Last 15 | ForEach-Object { Write-Output ('      ' + $_) }
  }
}

if ($fail -gt 0) {
  Write-Output ('共 ' + $fail + ' 个脚本失败')
  exit 1
}
Write-Output '全部通过（5 个脚本，覆盖分批批量、脚本语义、结构备份、导出五格式与 SQL 回放、监控降级）'