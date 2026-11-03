# 本地质量检查（发版前必须全绿）
#
# 用法：pwsh -File scripts/check.ps1 [-TmpDir <path>]
# 说明：只跑静态检查与单测，不产出构建物；完整矩阵交给 CI。
#
# -TmpDir：把编译与测试的临时目录指到别处。默认用系统 TEMP；当它所在盘
# 快满时，链接阶段会报 "There is not enough space on the disk"——那看起来
# 像代码错误，其实是磁盘问题。这个参数与下面那条前置检查就是为了把这种
# 误判挡在门口。

[CmdletBinding()]
param(
    [string]$TmpDir = ''
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# 链接一个 test 二进制大概要几百 MB；低于 2 GB 就基本必然失败。
$minFreeGB = 2

function Get-FreeGB([string]$path) {
    try {
        $full = [System.IO.Path]::GetFullPath($path)
        $drive = [System.IO.Path]::GetPathRoot($full)
        $d = New-Object System.IO.DriveInfo($drive)
        return [math]::Round($d.AvailableFreeSpace / 1GB, 2)
    } catch {
        return $null
    }
}

if ($TmpDir) {
    New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null
    $env:TMP = $TmpDir
    $env:TEMP = $TmpDir
    $env:GOTMPDIR = $TmpDir
    Write-Host "临时目录: $TmpDir（可用 $(Get-FreeGB $TmpDir) GB）" -ForegroundColor Cyan
}

$tmp = if ($env:GOTMPDIR) { $env:GOTMPDIR } elseif ($env:TEMP) { $env:TEMP } else { $env:TMP }
$free = Get-FreeGB $tmp
if ($null -ne $free -and $free -lt $minFreeGB) {
    Write-Host "临时目录所在盘只剩 $free GB：$tmp" -ForegroundColor Red
    Write-Host "编译/测试会在链接阶段报 'not enough space on the disk'，那看起来像代码错误。" -ForegroundColor Yellow
    Write-Host "先清理磁盘，或把临时目录指到别处：" -ForegroundColor Yellow
    Write-Host "  pwsh -File scripts/check.ps1 -TmpDir `$env:LOCALAPPDATA\Temp" -ForegroundColor Yellow
    exit 1
}

$env:CGO_ENABLED = '0'
$failed = @()

function Step([string]$name, [scriptblock]$action) {
    Write-Host "==> $name" -ForegroundColor Cyan
    & $action
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    FAILED: $name" -ForegroundColor Red
        $script:failed += $name
    } else {
        Write-Host "    ok" -ForegroundColor Green
    }
}

Step 'gofmt' {
    $out = gofmt -l .
    if ($out) {
        Write-Host "    以下文件未格式化：" -ForegroundColor Red
        $out | ForEach-Object { Write-Host "      $_" }
        $global:LASTEXITCODE = 1
    } else {
        $global:LASTEXITCODE = 0
    }
}

Step 'go vet' { go vet ./... }

Step 'go build' { go build ./... }

Step 'go test' { go test ./... }

# -race 需要 CGO，与「免 CGO」的构建目标冲突。
# 本机没有 C 工具链时跳过，由 CI（Linux，带 gcc）负责这一层。
$hasCC = $null -ne (Get-Command gcc -ErrorAction SilentlyContinue)
if ($hasCC) {
    Step 'go test -race' {
        $prev = $env:CGO_ENABLED
        $env:CGO_ENABLED = '1'
        go test -race ./...
        $env:CGO_ENABLED = $prev
    }
} else {
    Write-Host '==> go test -race' -ForegroundColor Yellow
    Write-Host '    skipped: 本机无 C 工具链（-race 需要 CGO），由 CI 覆盖' -ForegroundColor Yellow
}

Write-Host ''
if ($failed.Count -gt 0) {
    Write-Host ("检查失败: " + ($failed -join ', ')) -ForegroundColor Red
    exit 1
}
Write-Host '全部检查通过' -ForegroundColor Green
