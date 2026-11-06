# 兼容老命令：等价于 scripts\build.ps1 -All（三个平台全编）
#
#   powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\build-all.ps1 -Targets "windows/amd64"
#
# 具体编译逻辑都在 scripts\build.ps1，这里只做转发，避免两份实现漂移。
param(
  [string[]]$Targets = @()
)

$ErrorActionPreference = "Stop"
$build = Join-Path $PSScriptRoot "build.ps1"
if (-not (Test-Path $build)) { throw "找不到 $build" }

if ($Targets -and $Targets.Count -gt 0) {
  & $build -Targets $Targets
} else {
  & $build -All
}
exit $LASTEXITCODE
