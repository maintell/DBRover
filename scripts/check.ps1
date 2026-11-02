# 本地质量检查（发版前必须全绿）
#
# 用法：pwsh -File scripts/check.ps1
# 说明：只跑静态检查与单测，不产出构建物；完整矩阵交给 CI。

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

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
