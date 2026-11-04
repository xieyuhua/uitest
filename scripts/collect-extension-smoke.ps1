# 插件录制 → 控制台删接口 → 用剩下的建任务 → 运行入库（端到端自检）
#
#   powershell -ExecutionPolicy Bypass -File scripts\collect-extension-smoke.ps1
#
# 用独立端口（8499）与独立数据目录（data-ext-smoke），不影响正式数据；跑完自动清理。
# 说明：这里用 API 模拟插件的上报，验证"会话创建 → 上报 → 筛选 → 删除 → 建任务 → 入库"整条链路；
#      真正的浏览器侧钩子（net-hook.js）由 cmd 工具/人工加载插件验证。
param([int]$Port = 8499)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$base = "http://127.0.0.1:$Port"
$dataDir = Join-Path $root "data-ext-smoke"
$proc = $null

function Fail($msg) {
  Write-Host "插件录制自检失败：$msg" -ForegroundColor Red
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  exit 1
}

function Send($method, $path, $json) {
  $params = @{ Uri = "$base$path"; Method = $method; TimeoutSec = 120 }
  if ($json) {
    $params.Body = [Text.Encoding]::UTF8.GetBytes($json)
    $params.ContentType = "application/json; charset=utf-8"
  }
  return Invoke-RestMethod @params
}

try {
  Write-Host "[1/6] 编译并启动服务（端口 $Port，数据目录 data-ext-smoke）…" -ForegroundColor Cyan
  go build -o (Join-Path $root "uitest-server-ext.exe") .
  if ($LASTEXITCODE -ne 0) { Fail "编译失败" }
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
  $env:UITEST_DATA_DIR = $dataDir
  $proc = Start-Process -FilePath (Join-Path $root "uitest-server-ext.exe") `
    -ArgumentList @("-port", "$Port") -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $root "data-ext-smoke.log") `
    -RedirectStandardError (Join-Path $root "data-ext-smoke.err.log")
  $ready = $false
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    try { $null = Invoke-RestMethod "$base/api/health" -TimeoutSec 3; $ready = $true; break } catch { }
  }
  if (-not $ready) { Fail "服务未就绪，见 data-ext-smoke.err.log" }

  # ---------- 1. 插件侧：创建录制会话 ----------
  Write-Host "[2/6] 创建插件录制会话（source=extension）…" -ForegroundColor Cyan
  $cap = Send POST "/api/collect/captures" (@{
    source = "extension"
    start_url = "$base/demo/api-center.html"
    note = "extension-smoke"
    user_agent = "Mozilla/5.0 (extension smoke)"
  } | ConvertTo-Json)
  $cid = $cap.capture.id
  if (-not $cid) { Fail "没有拿到录制会话 id" }
  if ($cap.capture.source -ne "extension") { Fail "会话来源不是 extension" }

  # ---------- 2. 插件侧：上报录到的请求（含 1 条噪声 + 1 条重复上报）----------
  Write-Host "[3/6] 上报请求（4 条业务 + 1 条噪声，并重复上报一次验证幂等）…" -ForegroundColor Cyan
  # 用真实响应当"录到的样本"，这样字段映射与分页（total=23）都能被自动识别
  $sample = (Invoke-RestMethod "$base/demo/api/goods?page=1&size=3" -TimeoutSec 20) | ConvertTo-Json -Depth 8 -Compress
  $body = @{
    items = @(
      @{ id = "h1"; method = "GET"; url = "$base/demo/api/goods?page=1&size=3"; status = 200; type = "Fetch";
         mime_type = "application/json"; has_body = $true; body_size = $sample.Length; body_preview = $sample }
      @{ id = "h2"; method = "POST"; url = "$base/demo/api/goods"; status = 200; type = "XHR";
         mime_type = "application/json"; post_data = '{"page":1,"size":3}';
         has_body = $true; body_size = $sample.Length; body_preview = $sample }
      @{ id = "h3"; method = "GET"; url = "$base/api/monitor/heartbeat"; status = 204; type = "Fetch"; mime_type = ""; has_body = $false }
      @{ id = "h4"; method = "GET"; url = "$base/demo/api/info"; status = 200; type = "Fetch"; mime_type = "application/json"; has_body = $true; body_preview = '{"total":23}' }
    )
  } | ConvertTo-Json -Depth 6 -Compress
  $r1 = Send POST "/api/collect/captures/$cid/requests" $body
  Write-Host ("        第一次上报：新增 {0} 条，合计 {1} 条" -f $r1.added, $r1.total) -ForegroundColor DarkGray
  if ($r1.added -ne 4) { Fail "第一次上报应新增 4 条，实际 $($r1.added)" }
  $r2 = Send POST "/api/collect/captures/$cid/requests" $body
  if ($r2.added -ne 0) { Fail "重复上报不应重复计数（新增 $($r2.added)）" }

  # ---------- 3. 控制台：筛选 ----------
  Write-Host "[4/6] 控制台筛选（只看 GET / 排除 heartbeat）…" -ForegroundColor Cyan
  $f1 = Send GET "/api/collect/captures/${cid}?method=GET"
  Write-Host ("        method=GET → {0}/{1}" -f $f1.facets.kept, $f1.facets.total) -ForegroundColor DarkGray
  if ($f1.facets.kept -ne 3) { Fail "method=GET 应保留 3 条，实际 $($f1.facets.kept)" }
  $f2 = Send GET "/api/collect/captures/${cid}?exclude=heartbeat"
  if ($f2.facets.kept -ne 3) { Fail "exclude=heartbeat 应保留 3 条，实际 $($f2.facets.kept)" }

  # ---------- 4. 控制台：删除不想要的接口 ----------
  Write-Host "[5/6] 删除不想要的接口（先单条删噪声，再批量删两条）…" -ForegroundColor Cyan
  $d1 = Send DELETE "/api/collect/captures/$cid/requests" '{"ids":["h3"]}'
  Write-Host ("        单条删除：{0}" -f $d1.message) -ForegroundColor DarkGray
  if ($d1.deleted -ne 1 -or $d1.total -ne 3) { Fail "单条删除后应为 3 条，实际 $($d1.total)" }
  $again = Send POST "/api/collect/captures/$cid/requests" (@{ items = @(@{ id = "h3"; method = "GET"; url = "$base/api/monitor/heartbeat"; status = 204; type = "Fetch" }) } | ConvertTo-Json -Depth 5 -Compress)
  if ($again.added -ne 0) { Fail "已删除的条目不应被插件重发又加回来" }
  $d2 = Send DELETE "/api/collect/captures/$cid/requests" '{"ids":["h2","h4"]}'
  Write-Host ("        批量删除：{0}" -f $d2.message) -ForegroundColor DarkGray
  if ($d2.total -ne 1) { Fail "批量删除后应只剩 1 条，实际 $($d2.total)" }

  # ---------- 5. 用剩下的接口建任务并运行 ----------
  Write-Host "[6/6] 用剩下的 1 条接口建采集任务并运行（示例数据 23 条）…" -ForegroundColor Cyan
  $task = Send POST "/api/collect/tasks" (@{
    name = "extension-smoke"
    start_url = "$base/demo/api-center.html"
    capture_id = $cid
    request_ids = @("h1")
    schedule = @{ mode = "manual" }
    throttle = @{ min_interval_ms = 300; jitter_percent = 0; timeout_ms = 15000; retries = 1; backoff_ms = 500; max_fail_streak = 3; max_pages_cap = 20 }
    sink = @{ driver = "sqlite"; table = "ext_rows"; mode = "append"; batch_size = 100 }
    session = @{ mode = "none" }
  } | ConvertTo-Json -Depth 6)
  $ep = $task.endpoints[0]
  Write-Host ("        接口：{0} {1}；分页：{2}" -f $ep.method, $ep.url, $ep.pagination.mode) -ForegroundColor DarkGray
  if ($ep.pagination.mode -ne "page") { Fail "分页没有自动识别（mode=$($ep.pagination.mode)）" }

  $run = Send POST "/api/collect/tasks/$($task.id)/run" "{}"
  $detail = $null
  for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Milliseconds 800
    $detail = Send GET "/api/collect/runs/$($run.id)"
    if ($detail.status -ne "queued" -and $detail.status -ne "running") { break }
  }
  if ($detail.status -ne "passed") { Fail ("运行状态 " + $detail.status + "，错误：" + $detail.error) }
  if ($detail.written -lt 20) { Fail ("只写入 {0} 行，分页或字段映射有问题" -f $detail.written) }

  $dbPath = Join-Path $dataDir "harvest\harvest.db"
  if (-not (Test-Path $dbPath)) { Fail "没有找到 SQLite 文件 $dbPath" }

  Write-Host ""
  Write-Host ("插件录制自检通过 ✅  上报 4 条 → 删到 1 条 → 建任务运行 → 写入 {0} 行" -f $detail.written) -ForegroundColor Green
  Write-Host ("  接口：{0}  停止原因：{1}" -f $ep.url, ($detail.endpoints[0].stop_reason))
  Write-Host ("  落库：{0}" -f $dbPath)
  Write-Host ""
  exit 0
}
catch {
  Fail $_.Exception.Message
}
finally {
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  Start-Sleep -Milliseconds 800
  Remove-Item -Force (Join-Path $root "uitest-server-ext.exe") -ErrorAction SilentlyContinue
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
}
