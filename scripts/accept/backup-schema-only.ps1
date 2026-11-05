# 结构备份（`backup create -type schema`）与恢复的端到端验收（docs/08 §2.1）。
#
# 这条验的是「一个只有结构的备份真的不含数据、恢复出来是空表、索引还在」：
# 桩测可以断言段里没有 INSERT，但「恢复后表在、行数为 0、独立索引也回来了」
# 只能靠真二进制 + 真 SQLite 跑一遍。顺带钉住两条拒绝理由：`-type data` 明确说
# 未实现；恢复不带 `-env` 时报的是「连接只读」，而不是让用户去查文件是否存在。
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$bin = Join-Path $root 'bin\dbrover.exe'
$w = Join-Path $root ('.tmp\backup-schema-' + (Get-Date -Format 'HHmmssfff'))
New-Item -ItemType Directory -Force -Path $w | Out-Null

$src = Join-Path $w 'src.db'
$sch = Join-Path $w 'schema.dbr'
$full = Join-Path $w 'full.dbr'
$dst = Join-Path $w 'dst.db'
$nl = [Environment]::NewLine

function Run([string[]]$argv) {
  $out = & $bin @argv 2>&1
  return @{ code = $LASTEXITCODE; text = ($out -join $nl) }
}

# 1) 造库：两张表 + 一条独立索引 + 两行数据
foreach ($sql in @(
    'CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)',
    'CREATE TABLE b (id INTEGER PRIMARY KEY)',
    'CREATE INDEX idx_a_v ON a(v)',
    "INSERT INTO a (id, v) VALUES (1, 'x')",
    "INSERT INTO a (id, v) VALUES (2, 'y')"
  )) {
  $r = Run @('exec', '-db', $src, '-sql', $sql, '-env', 'local')
  if ($r.code -ne 0) { throw ('造库失败：' + $sql + $nl + $r.text) }
}

# 2) 两种类型各备一份
$r = Run @('backup', 'create', '-db', $src, '-out', $full)
if ($r.code -ne 0) { throw ('full 备份失败：' + $r.text) }
$r = Run @('backup', 'create', '-db', $src, '-out', $sch, '-type', 'schema')
if ($r.code -ne 0) { throw ('schema 备份失败：' + $r.text) }
if ($r.text -notmatch '类型 schema') { throw ('创建输出里没写类型：' + $r.text) }

$inspect = Run @('backup', 'inspect', $sch)
if ($inspect.text -notmatch '仅结构') { throw ('inspect 没有标出「仅结构」：' + $inspect.text) }

$fullSize = (Get-Item $full).Length
$schSize = (Get-Item $sch).Length
if ($schSize -ge $fullSize) {
  throw ('结构备份（' + $schSize + ' B）不小于完整备份（' + $fullSize + ' B）：数据没被排除')
}
Write-Output ('两类备份：full=' + $fullSize + ' B / schema=' + $schSize + ' B')

# 3) 恢复结构备份到新库：表在、行 0、索引在
$r = Run @('restore', '-file', $sch, '-db', $dst, '-yes', '-env', 'local')
if ($r.code -ne 0) { throw ('恢复失败：' + $r.text) }
if ($r.text -notmatch '0 行') { throw ('恢复报告里应全是 0 行：' + $r.text) }

$n = Run @('exec', '-db', $dst, '-sql', 'SELECT COUNT(*) AS n FROM a', '-env', 'local')
if ($n.text -notmatch '(?m)^0\s*$') { throw ('恢复后 a 不是空表：' + $n.text) }

$ix = Run @('exec', '-db', $dst, '-sql', "SELECT name FROM sqlite_master WHERE type='index' AND name='idx_a_v'", '-env', 'local')
if ($ix.text -notmatch 'idx_a_v') { throw ('独立索引没有恢复：' + $ix.text) }

# 用计数断言而不是列表匹配：exec 的输出把多行压成一行（表头 + 值），
# 拿 ^a$ 去匹配只会得到「表格显示格式变了」这种假失败。
$tabs = Run @('exec', '-db', $dst, '-sql', "SELECT COUNT(*) AS n FROM sqlite_master WHERE type='table' AND name IN ('a','b')", '-env', 'local')
if ($tabs.text -notmatch '(?m)^2\s*$') { throw ('两张表没有都恢复：' + $tabs.text) }
Write-Output '结构备份恢复：2 张表 + 独立索引都在，行数为 0'

# 4) 两条拒绝理由
$data = Run @('backup', 'create', '-db', $src, '-out', (Join-Path $w 'x.dbr'), '-type', 'data')
if ($data.code -eq 0) { throw '-type data 不该被接受' }
if ($data.text -notmatch '未实现') { throw ('-type data 的拒绝理由不对：' + $data.text) }

$noEnv = Run @('restore', '-file', $sch, '-db', (Join-Path $w 'dst2.db'), '-yes')
if ($noEnv.code -eq 0) { throw '不带 -env 的恢复不该成功' }
if ($noEnv.text -notmatch '只读') { throw ('不带 -env 时应说清是只读，实际：' + $noEnv.text) }
Write-Output '拒绝理由：data 说清未实现；恢复不带 -env 说清只读'

Write-Output '全部通过'