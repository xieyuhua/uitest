# 文件类型落库自检：Excel（.xlsx）/ JSON（.json）生成 + 下载 + upsert 不重复
#
#   powershell -ExecutionPolicy Bypass -File scripts\collect-files-smoke.ps1
#
# 独立端口（8496）与数据目录（data-files-smoke），跑完自动清理。
# 示例接口固定 23 条：两个任务各跑两次（第二次 upsert），文件里始终应该是 23 条。
param([int]$Port = 8496)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

$base = "http://127.0.0.1:$Port"
$dataDir = Join-Path $root "data-files-smoke"
$proc = $null
$expect = 23

function Fail($msg) {
  Write-Host "文件落库自检失败：$msg" -ForegroundColor Red
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

function NewFileTask($driver, $table) {
  $ep = @{
    id = "ep_$table"; name = "goods"; method = "GET"; enabled = $true
    url = "$base/demo/api/goods?page=1&size=5"; records_path = "data.list"
    pagination = @{ mode = "page"; param = "page"; in = "query"; start = 1; page_size = 5; max_pages = 20; total_path = "data.total"; stop_on_short = $true }
    fields = @(
      @{ column = "id"; path = "id" },
      @{ column = "name"; path = "name" },
      @{ column = "price"; path = "price" },
      @{ column = "owner_name"; path = "owner.name" }
    )
  }
  $body = @{
    name = "$driver task"; endpoints = @($ep); schedule = @{ mode = "manual" }
    throttle = @{ min_interval_ms = 250; jitter_percent = 0; timeout_ms = 15000; retries = 1; backoff_ms = 400; max_fail_streak = 3; max_pages_cap = 20 }
    sink = @{ driver = $driver; table = $table; mode = "upsert"; key_column = "id"; batch_size = 100 }
    session = @{ mode = "none" }
  } | ConvertTo-Json -Depth 8
  return Send POST "/api/collect/tasks" $body
}

function RunOnce($taskId, $label) {
  $run = Send POST "/api/collect/tasks/$taskId/run" "{}"
  for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Milliseconds 700
    $d = Send GET "/api/collect/runs/$($run.id)"
    if ($d.status -ne "queued" -and $d.status -ne "running") {
      if ($d.status -ne "passed") { Fail "$label 运行失败：$($d.error)" }
      return $d
    }
  }
  Fail "$label 运行超时"
}

try {
  Write-Host "[1/5] 编译并启动服务（端口 $Port，数据目录 data-files-smoke）…" -ForegroundColor Cyan
  go build -o (Join-Path $root "uitest-server-files.exe") .
  if ($LASTEXITCODE -ne 0) { Fail "编译失败" }
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
  $env:UITEST_DATA_DIR = $dataDir
  $proc = Start-Process -FilePath (Join-Path $root "uitest-server-files.exe") `
    -ArgumentList @("-port", "$Port") -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $root "data-files-smoke.log") `
    -RedirectStandardError (Join-Path $root "data-files-smoke.err.log")
  $ready = $false
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 500
    try { $null = Invoke-RestMethod "$base/api/health" -TimeoutSec 3; $ready = $true; break } catch { }
  }
  if (-not $ready) { Fail "服务未就绪，见 data-files-smoke.err.log" }

  Write-Host "[2/5] Excel 任务：跑两次（第二次 upsert 不重复）…" -ForegroundColor Cyan
  $t1 = NewFileTask "xlsx" "goods_xlsx"
  $r1 = RunOnce $t1.id "xlsx#1"
  $r2 = RunOnce $t1.id "xlsx#2"
  Write-Host ("        写入 {0} / {1} 行（upsert 语义：第二次覆盖不追加）" -f $r1.written, $r2.written) -ForegroundColor DarkGray

  Write-Host "[3/5] JSON 任务：跑两次（第二次 upsert 不重复）…" -ForegroundColor Cyan
  $t2 = NewFileTask "json" "goods_json"
  $j1 = RunOnce $t2.id "json#1"
  $j2 = RunOnce $t2.id "json#2"
  Write-Host ("        写入 {0} / {1} 行" -f $j1.written, $j2.written) -ForegroundColor DarkGray

  Write-Host "[4/5] 产出文件列表 + 下载…" -ForegroundColor Cyan
  $files = Send GET "/api/collect/files"
  foreach ($f in $files.items) {
    Write-Host ("        {0}  {1}  {2} 字节" -f $f.kind, $f.name, $f.size) -ForegroundColor DarkGray
  }
  $xlsx = $files.items | Where-Object { $_.name -eq "goods_xlsx.xlsx" }
  $json = $files.items | Where-Object { $_.name -eq "goods_json.json" }
  if (-not $xlsx) { Fail "没有找到 goods_xlsx.xlsx" }
  if (-not $json) { Fail "没有找到 goods_json.json" }

  $dl = Join-Path $dataDir "dl"
  New-Item -ItemType Directory -Force -Path $dl | Out-Null
  Invoke-WebRequest "$base/api/collect/files/goods_xlsx.xlsx" -OutFile (Join-Path $dl "goods_xlsx.xlsx") -TimeoutSec 30
  Invoke-WebRequest "$base/api/collect/files/goods_json.json" -OutFile (Join-Path $dl "goods_json.json") -TimeoutSec 30
  $xb = [System.IO.File]::ReadAllBytes((Join-Path $dl "goods_xlsx.xlsx"))
  if ($xb.Length -lt 1000) { Fail "下载的 xlsx 太小（$($xb.Length) 字节）" }
  if (-not ($xb[0] -eq 0x50 -and $xb[1] -eq 0x4B)) { Fail "下载的 xlsx 不是 zip（Excel 打不开）" }

  Write-Host "[5/6] 校验内容行数…" -ForegroundColor Cyan
  $jsonText = [System.IO.File]::ReadAllText((Join-Path $dl "goods_json.json"), [Text.Encoding]::UTF8)
  $rows = $jsonText | ConvertFrom-Json
  if ($rows.Count -ne $expect) { Fail "json 里 $($rows.Count) 条，期望 $expect（upsert 可能失效）" }
  $pick = $rows | Where-Object { $_.id -eq 1023 }
  if (-not $pick) { Fail "json 里找不到 id=1023 的记录" }

  # xlsx 用 python + openpyxl 校验（有就跑，没有就只看 zip 头）
  $py = $null
  foreach ($cand in @(
      "C:\Program Files\DSH Desktop\resources\office-runtime\primary-runtime\dependencies\python\python.exe",
      "python", "python3")) {
    if ($cand -eq "python" -or $cand -eq "python3") {
      $cmd = Get-Command $cand -ErrorAction SilentlyContinue
      if ($cmd) { $py = $cmd.Source; break }
    } elseif (Test-Path $cand) { $py = $cand; break }
  }
  if ($py) {
    $script = @'
import sys
from openpyxl import load_workbook
wb = load_workbook(sys.argv[1])
ws = wb.active
rows = ws.max_row - 1
print("XLSX_ROWS=%d" % rows)
print("XLSX_HEADER=%s" % ",".join(str(c.value) for c in ws[1]))
print("XLSX_LAST_ID=%s" % ws.cell(row=ws.max_row, column=2).value)
'@
    $pyFile = Join-Path $dl "check_xlsx.py"
    [System.IO.File]::WriteAllText($pyFile, $script, (New-Object System.Text.UTF8Encoding($false)))
    $out = & $py $pyFile (Join-Path $dl "goods_xlsx.xlsx")
    $out | ForEach-Object { Write-Host ("        " + $_) -ForegroundColor DarkGray }
    $line = $out | Where-Object { $_ -like "XLSX_ROWS=*" }
    if ($line) {
      $n = [int]($line -replace "XLSX_ROWS=", "")
      if ($n -ne $expect) { Fail "xlsx 里 $n 行，期望 $expect（upsert 可能失效）" }
    }
  } else {
    Write-Host "        （没找到 python/openpyxl，跳过 xlsx 行数直查；zip 头已校验）" -ForegroundColor DarkGray
  }

  # ---------- 6. 清空数据 ----------
  Write-Host "[6/6] 清空数据（产出文件 / 任务 / 运行记录）…" -ForegroundColor Cyan
  try { $null = Send DELETE "/api/collect/files" } catch {
    if ($_.Exception.Response.StatusCode.value__ -ne 400) { Fail "清空文件不带 all=1 应返回 400" }
    Write-Host "        未带 all=1 → 400（符合预期）" -ForegroundColor DarkGray
  }
  $c1 = Send DELETE "/api/collect/files?all=1"
  Write-Host ("        清空产出文件：{0}" -f $c1.message) -ForegroundColor DarkGray
  if ($c1.deleted -lt 2) { Fail "应至少删掉 2 个产出文件，实际 $($c1.deleted)" }
  $left = (Send GET "/api/collect/files").items
  if ($left.Count -ne 0) { Fail "清空后还剩下 $($left.Count) 个文件" }

  $c2 = Send DELETE "/api/collect/tasks?all=1"
  Write-Host ("        清空任务：{0}" -f $c2.message) -ForegroundColor DarkGray
  if ((Send GET "/api/collect/tasks").items.Count -ne 0) { Fail "清空后还有采集任务" }
  if ((Send GET "/api/collect/runs").items.Count -ne 0) { Fail "清空任务后应连带清掉运行记录" }

  Write-Host ""
  Write-Host ("文件落库自检通过 ✅  Excel + JSON 各 {0} 条，均可下载；清空数据可用" -f $expect) -ForegroundColor Green
  Write-Host ("  文件目录：{0}" -f (Join-Path $dataDir "harvest"))
  Write-Host ""
  exit 0
}
catch {
  Fail $_.Exception.Message
}
finally {
  if ($proc -and -not $proc.HasExited) { $proc.Kill() }
  Start-Sleep -Milliseconds 800
  Remove-Item -Force (Join-Path $root "uitest-server-files.exe") -ErrorAction SilentlyContinue
  if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue }
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
}
