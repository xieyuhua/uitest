# 接口采集端到端自检：录制示例接口 → 建任务 → 执行 → 校验落库行数
#
#   powershell -ExecutionPolicy Bypass -File scripts\collect-smoke.ps1
#
# 用独立端口与独立数据目录（data-smoke），不影响正式数据；跑完自动清理。
param([int]$Port = 8497)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$base = "http://127.0.0.1:$Port"
$dataDir = Join-Path $root "data-smoke"
$proc = $null

function Fail($msg) {
  Write-Host "接口采集自检失败：$msg" -ForegroundColor Red
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  exit 1
}

try {
  Write-Host "[1/6] 编译…" -ForegroundColor Cyan
  go build -o (Join-Path $root "uitest-server-smoke.exe") .
  if ($LASTEXITCODE -ne 0) { Fail "编译失败" }

  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
  $env:UITEST_DATA_DIR = $dataDir
  Write-Host "[2/6] 启动服务（端口 $Port，数据目录 data-smoke）…" -ForegroundColor Cyan
  $proc = Start-Process -FilePath (Join-Path $root "uitest-server-smoke.exe") `
    -ArgumentList @("-port", "$Port") -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $root "data-smoke.log") `
    -RedirectStandardError (Join-Path $root "data-smoke.err.log")

  $ok = $false
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    try { $null = Invoke-RestMethod "$base/api/health" -TimeoutSec 3; $ok = $true; break } catch { }
  }
  if (-not $ok) { Fail "服务未就绪，见 data-smoke.err.log" }

  Write-Host "[3/6] 录制示例接口页（会启动内置浏览器，约 5-15 秒）…" -ForegroundColor Cyan
  $capBody = @{
    start_url      = "$base/demo/api-center.html"
    types          = @("XHR", "Fetch")
    capture_bodies = $true
    note           = "collect-smoke"
  } | ConvertTo-Json
  $cap = Invoke-RestMethod "$base/api/collect/captures" -Method Post -Body $capBody -ContentType "application/json" -TimeoutSec 180
  $capId = $cap.capture.id
  if (-not $capId) { Fail "没有拿到录制会话 id" }

  Start-Sleep -Seconds 6
  $stop = Invoke-RestMethod "$base/api/collect/captures/$capId/stop" -Method Post -Body "{}" -ContentType "application/json" -TimeoutSec 60
  if (-not $stop.items -or $stop.items.Count -lt 1) { Fail "没有录到任何请求（内置浏览器是否可用？）" }
  $req = $stop.items | Where-Object { $_.path -like "*goods*" } | Select-Object -First 1
  if (-not $req) { Fail "没有录到 demo 的分页列表接口 /demo/api/goods" }
  Write-Host ("        录到 {0} 条，选中：{1} {2}" -f $stop.items.Count, $req.method, $req.path) -ForegroundColor DarkGray

  Write-Host "[4/6] 新建采集任务（SQLite 落库 / 仅手动）…" -ForegroundColor Cyan
  $taskBody = @{
    name        = "collect-smoke"
    start_url   = "$base/demo/api-center.html"
    capture_id  = $capId
    request_ids = @($req.id)
    schedule    = @{ mode = "manual" }
    throttle    = @{ min_interval_ms = 400; jitter_percent = 0; timeout_ms = 15000; retries = 1; backoff_ms = 500; max_fail_streak = 3; max_pages_cap = 10 }
    sink        = @{ driver = "sqlite"; table = "smoke_items"; mode = "append"; batch_size = 100 }
    session     = @{ mode = "none" }
  } | ConvertTo-Json -Depth 6
  $task = Invoke-RestMethod "$base/api/collect/tasks" -Method Post -Body $taskBody -ContentType "application/json" -TimeoutSec 60
  if (-not $task.id) { Fail "任务创建失败" }
  Write-Host ("        任务 {0}：{1} 个接口，计划 {2}" -f $task.id, $task.endpoint_count, $task.schedule_text) -ForegroundColor DarkGray

  Write-Host "[5/6] 试抓预览（不写库）…" -ForegroundColor Cyan
  $prev = Invoke-RestMethod "$base/api/collect/tasks/$($task.id)/preview" -Method Post -Body '{"limit":3}' -ContentType "application/json" -TimeoutSec 120
  $p0 = $prev.items | Select-Object -First 1
  if (-not $p0 -or $p0.error) { Fail ("试抓失败：" + $(if ($p0) { $p0.error } else { "无返回" })) }
  Write-Host ("        记录路径 {0}，识别列：{1}" -f $p0.records_path, ($p0.columns -join ",")) -ForegroundColor DarkGray

  Write-Host "[6/6] 执行采集并校验落库…" -ForegroundColor Cyan
  $run = Invoke-RestMethod "$base/api/collect/tasks/$($task.id)/run" -Method Post -Body "{}" -ContentType "application/json" -TimeoutSec 60
  $detail = $null
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 800
    $detail = Invoke-RestMethod "$base/api/collect/runs/$($run.id)" -TimeoutSec 30
    if ($detail.status -ne "queued" -and $detail.status -ne "running") { break }
  }
  if ($detail.status -ne "passed") { Fail ("运行状态 " + $detail.status + "，错误：" + $detail.error) }
  if ($detail.written -lt 1) { Fail "写入 0 行，检查落库配置" }
  # 示例数据固定 23 条：自动分页应该一次抓全，而不是只抓到第一页的 3 条
  if ($detail.written -lt 20) { Fail ("只写入 {0} 行，分页好像没生效（期望 >= 20 行）" -f $detail.written) }

  $dbPath = Join-Path $dataDir "harvest\harvest.db"
  if (-not (Test-Path $dbPath)) { Fail "没有找到 SQLite 文件 $dbPath" }
  $dbSize = (Get-Item $dbPath).Length
  $ep0 = $detail.endpoints | Select-Object -First 1

  Write-Host ""
  Write-Host ("接口采集自检通过 ✅  请求 {0} 次 / 解析 {1} 条 / 写入 {2} 行" -f $detail.requests, $detail.fetched, $detail.written) -ForegroundColor Green
  Write-Host ("  分页：{0}（停止原因：{1}）" -f $req.path, $ep0.stop_reason)
  Write-Host ("  落库：{0}（{1} 字节）" -f $dbPath, $dbSize)
  Write-Host ("  表结构：{0}" -f ($p0.columns -join ", "))
  Write-Host ""
  exit 0
}
catch {
  Fail $_.Exception.Message
}
finally {
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  Remove-Item -Force (Join-Path $root "uitest-server-smoke.exe") -ErrorAction SilentlyContinue
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
}
