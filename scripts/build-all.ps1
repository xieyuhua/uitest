# 一次编译出 Windows / Linux 的可执行文件（纯 Go，无需 CGO，无需目标机装依赖）
#
#   powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1
#
# 产物在 dist\ 下：
#   uitest-server-windows-amd64.exe
#   uitest-server-linux-amd64
#   uitest-server-linux-arm64
param(
  [string[]]$Targets = @("windows/amd64", "linux/amd64", "linux/arm64")
)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$dist = Join-Path $root "dist"
New-Item -ItemType Directory -Force -Path $dist | Out-Null

$oldGOOS = $env:GOOS
$oldGOARCH = $env:GOARCH
$oldCGO = $env:CGO_ENABLED

try {
  foreach ($t in $Targets) {
    $parts = $t.Split("/")
    $os = $parts[0]
    $arch = $parts[1]
    $name = "uitest-server-$os-$arch"
    if ($os -eq "windows") { $name += ".exe" }

    Write-Host "[build] $os/$arch → dist\$name" -ForegroundColor Cyan
    $env:GOOS = $os
    $env:GOARCH = $arch
    $env:CGO_ENABLED = "0"   # 数据库驱动全部纯 Go，关掉 CGO 才能免依赖交叉编译
    $sw = [Diagnostics.Stopwatch]::StartNew()
    go build -trimpath -ldflags "-s -w" -o (Join-Path $dist $name) .
    if ($LASTEXITCODE -ne 0) { throw "编译 $os/$arch 失败" }
    $size = [math]::Round((Get-Item (Join-Path $dist $name)).Length / 1MB, 1)
    Write-Host ("        完成：{0} MB，用时 {1:N1}s" -f $size, $sw.Elapsed.TotalSeconds) -ForegroundColor DarkGray
  }
} finally {
  $env:GOOS = $oldGOOS
  $env:GOARCH = $oldGOARCH
  $env:CGO_ENABLED = $oldCGO
}

Write-Host ""
Write-Host "全部完成。Windows 直接运行；Linux 拷贝后 chmod +x 即可（部署方式见 docs/COLLECT.md 第 10 章）。" -ForegroundColor Green
