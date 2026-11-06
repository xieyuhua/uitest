# 生成示例数据：7 条示例流程（63 步，覆盖 8 个示例页面）
#
#   powershell -ExecutionPolicy Bypass -File scripts\seed-demo.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\seed-demo.ps1 -Port 8490 -Only 登录
#
# 做了什么：确保服务端在跑 → 用内置浏览器逐个"建录制 → 交步骤 → 执行 → 自愈回写"
#          → 最后保留这些录制（-keep），于是控制台「录制」列表里就有示例可点了。
# 注意：data/ 是运行时目录、不入库，所以新克隆的仓库第一次是空的，跑这个脚本即可补齐演示数据。
param(
  [int]$Port = 8490,
  [string]$Only = ""
)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$base = "http://127.0.0.1:$Port"
$proc = $null

function Test-Server {
  try { $null = Invoke-RestMethod "$base/api/health" -TimeoutSec 3; return $true } catch { return $false }
}

try {
  if (-not (Test-Server)) {
    Write-Host "服务端没在 $base 上，先启动一个…" -ForegroundColor Cyan
    $exe = Join-Path $root "uitest-server.exe"
    if (-not (Test-Path $exe)) {
      go build -o $exe .
      if ($LASTEXITCODE -ne 0) { throw "编译失败" }
    }
    $proc = Start-Process -FilePath $exe -ArgumentList @("-port", "$Port") -PassThru -WindowStyle Hidden `
      -RedirectStandardOutput (Join-Path $root "data-seed.log") `
      -RedirectStandardError (Join-Path $root "data-seed.err.log")
    $ok = $false
    for ($i = 0; $i -lt 40; $i++) {
      Start-Sleep -Milliseconds 500
      if (Test-Server) { $ok = $true; break }
    }
    if (-not $ok) { throw "服务端没起来，见 data-seed.err.log" }
  }

  Write-Host "开始生成示例场景（会真的用内置浏览器跑一遍，大约 1-3 分钟）…" -ForegroundColor Cyan
  $scArgs = @("run", "./cmd/scenarios", "-base", $base, "-keep")
  if ($Only) { $scArgs += @("-only", $Only) }
  & go @scArgs
  if ($LASTEXITCODE -ne 0) { throw "场景生成失败（退出码 $LASTEXITCODE）" }

  $dataDir = if ($env:UITEST_DATA_DIR) { $env:UITEST_DATA_DIR } else { "data" }
  Write-Host ""
  Write-Host "示例数据已就绪 ✅ 打开 $base 的「录制」列表即可看到。" -ForegroundColor Green
  Write-Host "数据目录：$(Join-Path $root $dataDir)（运行时目录，不入库）"
}
finally {
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
  if ($proc -and -not $proc.HasExited) {
    Write-Host "关闭临时启动的服务端（PID $($proc.Id)）…" -ForegroundColor DarkGray
    $proc.Kill()
  }
}
