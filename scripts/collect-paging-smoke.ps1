# 分页采集端到端自检：5 种分页形态各跑一遍，校验"到底有没有抓全"
#
#   powershell -ExecutionPolicy Bypass -File scripts\collect-paging-smoke.ps1
#
# 用独立端口（8498）与独立数据目录（data-paging），不影响正式数据；跑完自动清理。
# 示例接口固定 23 条数据（internal/demoapi），所以"抓全"= 写入 23 行。
param([int]$Port = 8498)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$base = "http://127.0.0.1:$Port"
$dataDir = Join-Path $root "data-paging"
$expectTotal = 23
$proc = $null

function Fail($msg) {
  Write-Host "分页采集自检失败：$msg" -ForegroundColor Red
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  exit 1
}

function Post($path, $obj) {
  $json = if ($obj -is [string]) { $obj } else { $obj | ConvertTo-Json -Depth 8 -Compress }
  $bytes = [Text.Encoding]::UTF8.GetBytes($json)
  return Invoke-RestMethod "$base$path" -Method Post -Body $bytes -ContentType "application/json; charset=utf-8" -TimeoutSec 120
}

function Patch($path, $obj) {
  $json = $obj | ConvertTo-Json -Depth 8 -Compress
  $bytes = [Text.Encoding]::UTF8.GetBytes($json)
  return Invoke-RestMethod "$base$path" -Method Patch -Body $bytes -ContentType "application/json; charset=utf-8" -TimeoutSec 60
}

function NewTask($name, $endpoints, $maxPages = 20) {
  $body = @{
    name       = $name
    endpoints  = $endpoints
    schedule   = @{ mode = "manual" }
    throttle   = @{ min_interval_ms = 250; jitter_percent = 0; timeout_ms = 15000; retries = 1; backoff_ms = 400; max_fail_streak = 3; max_pages_cap = $maxPages }
    sink       = @{ driver = "sqlite"; table = "paging_rows"; mode = "append"; batch_size = 100 }
    session    = @{ mode = "none" }
  }
  return Post "/api/collect/tasks" $body
}

function RunTask($taskId) {
  $run = Post "/api/collect/tasks/$taskId/run" "{}"
  for ($i = 0; $i -lt 80; $i++) {
    Start-Sleep -Milliseconds 700
    $d = Invoke-RestMethod "$base/api/collect/runs/$($run.id)" -TimeoutSec 30
    if ($d.status -ne "queued" -and $d.status -ne "running") { return $d }
  }
  Fail "运行超时：$taskId"
}

function Check($label, $detail, $expectWritten, [switch]$AllowLess) {
  $ep = $detail.endpoints | Select-Object -First 1
  $ok = if ($AllowLess) { $detail.written -gt 0 -and $detail.written -le $expectWritten }
        else { $detail.written -eq $expectWritten }
  $flag = if ($ok) { "OK " } else { "BAD" }
  Write-Host ("  [{0}] {1,-16} 页数={2} 解析={3} 写入={4} / 期望{5}  停止原因={6}" -f `
    $flag, $label, $detail.requests, $detail.fetched, $detail.written, $expectWritten, $ep.stop_reason) -ForegroundColor DarkGray
  if (-not $ok) { Fail "$label 写入行数不对（$($detail.written) != $expectWritten）" }
  return $detail
}

try {
  Write-Host "[1/7] 编译并启动服务（端口 $Port，数据目录 data-paging）…" -ForegroundColor Cyan
  go build -o (Join-Path $root "uitest-server-paging.exe") .
  if ($LASTEXITCODE -ne 0) { Fail "编译失败" }
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
  $env:UITEST_DATA_DIR = $dataDir
  $proc = Start-Process -FilePath (Join-Path $root "uitest-server-paging.exe") `
    -ArgumentList @("-port", "$Port") -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $root "data-paging.log") `
    -RedirectStandardError (Join-Path $root "data-paging.err.log")
  $ready = $false
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    try { $null = Invoke-RestMethod "$base/api/health" -TimeoutSec 3; $ready = $true; break } catch { }
  }
  if (-not $ready) { Fail "服务未就绪，见 data-paging.err.log" }

  $info = Invoke-RestMethod "$base/demo/api/info" -TimeoutSec 10
  Write-Host ("        示例接口固定 " + $info.total + " 条数据，下面按分页形态逐个验证") -ForegroundColor DarkGray

  # ---------- 场景 A：录制 → 自动识别分页 → 抓全 ----------
  Write-Host "[2/7] 场景 A：录制示例页（page=1&size=3）→ 自动识别分页 → 运行" -ForegroundColor Cyan
  $cap = Post "/api/collect/captures" @{
    start_url = "$base/demo/api-center.html"; types = @("XHR", "Fetch"); capture_bodies = $true; note = "paging"
  }
  Start-Sleep -Seconds 6
  $stop = Post "/api/collect/captures/$($cap.capture.id)/stop" "{}"
  $req = $stop.items | Where-Object { $_.path -like "*goods*" } | Select-Object -First 1
  if (-not $req) { Fail "没有录到 /demo/api/goods 请求" }
  $task = Post "/api/collect/tasks" @{
    name = "A 自动识别分页"; start_url = "$base/demo/api-center.html"
    capture_id = $cap.capture.id; request_ids = @($req.id)
    schedule = @{ mode = "manual" }
    throttle = @{ min_interval_ms = 250; jitter_percent = 0; timeout_ms = 15000; retries = 1; backoff_ms = 400; max_fail_streak = 3; max_pages_cap = 20 }
    sink = @{ driver = "sqlite"; table = "paging_rows"; mode = "append"; batch_size = 100 }
    session = @{ mode = "none" }
  }
  $autoPag = $task.endpoints[0].pagination
  Write-Host ("        自动识别结果：mode={0} param={1} page_size={2} total_path={3} max_pages={4}" -f `
      $autoPag.mode, $autoPag.param, $autoPag.page_size, $autoPag.total_path, $autoPag.max_pages) -ForegroundColor DarkGray
  if ($autoPag.mode -ne "page" -or $autoPag.param -ne "page") { Fail "分页没识别对：$($autoPag.mode)/$($autoPag.param)" }
  $d = RunTask $task.id
  Check "A 自动识别" $d $expectTotal | Out-Null

  # ---------- 场景 A2：最多页数不被任务的默认上限压回去 ----------
  Write-Host "[2/7+] 场景 A2：接口填 500 页，任务默认上限 50 → 应保留 500" -ForegroundColor Cyan
  $tA2 = NewTask "A2 maxpages" @(@{
    id = "ep_a2"; name = "pagemax"; method = "GET"; enabled = $true
    url = "$base/demo/api/goods?page=1&size=5"; records_path = "data.list"
    pagination = @{ mode = "page"; param = "page"; in = "query"; start = 1; page_size = 5; max_pages = 500; total_path = "data.total"; stop_on_short = $true }
    fields = @(@{ column = "id"; path = "id" }, @{ column = "name"; path = "name" })
  }) 50
  $gotMax = $tA2.endpoints[0].pagination.max_pages
  Write-Host ("        接口最多页数 = {0}（任务默认上限 50）" -f $gotMax) -ForegroundColor DarkGray
  if ($gotMax -ne 500) { Fail "最多页数 500 被压成了 $gotMax（应原样保留）" }
  # 留空（0）时应回落到任务的默认上限
  $tA3 = NewTask "A3 autocap" @(@{
    id = "ep_a3"; name = "autocap"; method = "GET"; enabled = $true
    url = "$base/demo/api/goods?page=1&size=5"; records_path = "data.list"
    pagination = @{ mode = "page"; param = "page"; in = "query"; start = 1; page_size = 5; max_pages = 0; total_path = "data.total"; stop_on_short = $true }
    fields = @(@{ column = "id"; path = "id" })
  }) 120
  $gotAuto = $tA3.endpoints[0].pagination.max_pages
  Write-Host ("        留空时 = {0}（任务默认上限 120）" -f $gotAuto) -ForegroundColor DarkGray
  if ($gotAuto -ne 120) { Fail "留空时应用任务默认上限 120，实际 $gotAuto" }

  # ---------- 场景 B：偏移分页 ----------
  Write-Host "[3/7] 场景 B：偏移分页 offset/limit" -ForegroundColor Cyan
  $tB = NewTask "B offset" @(@{
    id = "ep_b"; name = "offset"; method = "GET"; enabled = $true
    url = "$base/demo/api/goods-offset?offset=0&limit=5"; records_path = "data.list"
    pagination = @{ mode = "offset"; param = "offset"; in = "query"; start = 0; page_size = 5; max_pages = 20; total_path = "data.total"; stop_on_short = $true }
    fields = @(@{ column = "id"; path = "id" }, @{ column = "name"; path = "name" })
  })
  Check "B offset" (RunTask $tB.id) $expectTotal | Out-Null

  # ---------- 场景 C：游标分页 ----------
  Write-Host "[4/7] 场景 C：游标分页 cursor/next_cursor" -ForegroundColor Cyan
  $tC = NewTask "C cursor" @(@{
    id = "ep_c"; name = "cursor"; method = "GET"; enabled = $true
    url = "$base/demo/api/goods-cursor?cursor=&limit=5"; records_path = "data.list"
    pagination = @{ mode = "cursor"; param = "cursor"; in = "query"; page_size = 5; max_pages = 20; next_path = "data.next_cursor"; has_more_path = "data.has_more"; stop_on_short = $true }
    fields = @(@{ column = "id"; path = "id" }, @{ column = "name"; path = "name" })
  })
  Check "C cursor" (RunTask $tC.id) $expectTotal | Out-Null

  # ---------- 场景 D：分页参数在请求体 ----------
  Write-Host "[5/7] 场景 D：POST 请求体里的 page/size" -ForegroundColor Cyan
  $tD = NewTask "D body" @(@{
    id = "ep_d"; name = "body paging"; method = "POST"; enabled = $true
    url = "$base/demo/api/goods"; body = '{"page":1,"size":5}'; records_path = "data.list"
    pagination = @{ mode = "page"; param = "page"; in = "body"; start = 1; page_size = 5; max_pages = 20; total_path = "data.total"; stop_on_short = $true }
    fields = @(@{ column = "id"; path = "id" }, @{ column = "name"; path = "name" })
  })
  Check "D body" (RunTask $tD.id) $expectTotal | Out-Null

  # ---------- 场景 E：假分页（接口忽略页码参数）----------
  Write-Host "[6/7] 场景 E：接口忽略分页参数时的防死循环" -ForegroundColor Cyan
  $tE = NewTask "E fake paging" @(@{
    id = "ep_e"; name = "static json"; method = "GET"; enabled = $true
    url = "$base/demo/api/items.json?page=1&size=3"; records_path = "data.list"
    pagination = @{ mode = "page"; param = "page"; in = "query"; start = 1; page_size = 3; max_pages = 10; stop_on_short = $false }
    fields = @(@{ column = "id"; path = "id" }, @{ column = "name"; path = "name" })
  })
  $dE = RunTask $tE.id
  $epE = $dE.endpoints | Select-Object -First 1
  Write-Host ("        停止原因：{0}；页数={1}，写入={2}" -f $epE.stop_reason, $dE.requests, $dE.written) -ForegroundColor DarkGray
  if ($dE.requests -gt 3) { Fail "没有及时停止（打了 $($dE.requests) 页），防死循环没生效" }
  if ($dE.written -ne 6) { Fail "重复数据没去重（写入 $($dE.written) 行，应为 6）" }

  # ---------- 场景 F：落库校验 ----------
  Write-Host "[7/7] 校验数据库里的数据量" -ForegroundColor Cyan
  $dbPath = Join-Path $dataDir "harvest\harvest.db"
  if (-not (Test-Path $dbPath)) { Fail "没有找到 SQLite 文件 $dbPath" }
  $sqlite = Get-Command sqlite3 -ErrorAction SilentlyContinue
  if ($sqlite) {
    $count = & $sqlite.Source $dbPath "select count(*) from paging_rows;" 2>$null
    # append 模式：A/B/C/D 各 23 行 + E 的 6 行 = 98（E 只用来验证"不会重复抓同一页"）
    Write-Host ("        paging_rows 共 {0} 行（A/B/C/D 各 {1} 行 + E 6 行，append 累加）" -f $count, $expectTotal) -ForegroundColor DarkGray
    if ([int]$count -ne ($expectTotal * 4 + 6)) { Fail "总行数不对：$count" }
  } else {
    Write-Host "        （本机没有 sqlite3 命令，跳过行数直查；上面的接口返回已包含写入行数）" -ForegroundColor DarkGray
  }

  Write-Host ""
  Write-Host ("分页采集自检通过 ✅  5 种形态都能抓全（示例数据 {0} 条）；最多页数按填写值生效" -f $expectTotal) -ForegroundColor Green
  Write-Host "  A 自动识别 / B 偏移 / C 游标 / D 请求体 / E 假分页防护"
  Write-Host ""
  exit 0
}
catch {
  Fail $_.Exception.Message
}
finally {
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  Get-CimInstance Win32_Process -Filter "Name='msedge.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandLine -match 'data-paging' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
  Start-Sleep -Milliseconds 800
  Remove-Item -Force (Join-Path $root "uitest-server-paging.exe") -ErrorAction SilentlyContinue
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
}
