# 编译并后台启动 uitest 服务端，等待健康检查通过后打印入口地址
param([int]$Port = 8490)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $root

Get-Process uitest-server -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 400

Write-Host "[1/3] 编译…" -ForegroundColor Cyan
$env:GOFLAGS = "-mod=mod"
go build -o uitest-server.exe .
if ($LASTEXITCODE -ne 0) { throw "编译失败" }

Write-Host "[2/3] 启动（端口 $Port）…" -ForegroundColor Cyan
$exeArgs = @()
if ($Port -ne 8490) { $exeArgs = @("-port", "$Port") }
if ($exeArgs.Count -gt 0) {
  Start-Process -FilePath ".\uitest-server.exe" -ArgumentList $exeArgs -RedirectStandardOutput "data-start.log" -RedirectStandardError "data-start.err.log" -WindowStyle Hidden
} else {
  Start-Process -FilePath ".\uitest-server.exe" -RedirectStandardOutput "data-start.log" -RedirectStandardError "data-start.err.log" -WindowStyle Hidden
}

Write-Host "[3/3] 健康检查…" -ForegroundColor Cyan
$base = "http://127.0.0.1:$Port"
for ($i = 0; $i -lt 30; $i++) {
  Start-Sleep -Milliseconds 500
  try {
    $h = (Invoke-WebRequest "$base/api/health" -UseBasicParsing -TimeoutSec 3).Content | ConvertFrom-Json
    Write-Host ""
    Write-Host "已启动：$base/" -ForegroundColor Green
    Write-Host "  控制台      $base/"
    Write-Host "  示例被测页  $base/demo/"
    Write-Host "  录制 {0} 条 / 运行 {1} 次 / 内置浏览器：{2}" -f $h.recordings, $h.runs, $(if ($h.browser_running) { "运行中" } else { "未启动（执行时自动拉起）" })
    Write-Host ""
    Write-Host "浏览器插件：edge://extensions → 开发人员模式 → 加载解压缩的扩展 → 选择 $root\extension" -ForegroundColor DarkGray
    exit 0
  } catch { }
}
Write-Host "启动失败，请查看 data-start.err.log" -ForegroundColor Red
exit 1