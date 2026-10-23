# 端到端自检：造一条「新建服务商品」的录制 → AI 生成数据 → 用内置浏览器真跑一遍 → 打印逐步结果 → 回写自愈定位
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\smoke.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\smoke.ps1 -Base http://127.0.0.1:8480 -Keep
param(
  [string]$Base = "http://127.0.0.1:8490",
  [switch]$Keep
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$utf8 = New-Object Text.UTF8Encoding($false)
$tmp = Join-Path $env:TEMP ("uitest-smoke-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

function Invoke-Api {
  param([string]$Method, [string]$Path, $Body)
  $curlArgs = @("-s", "-S", "-X", $Method, "-H", "Content-Type: application/json")
  if ($null -ne $Body) {
    $file = Join-Path $tmp ([guid]::NewGuid().ToString("N") + ".json")
    if ($Body -is [string]) { $json = $Body } else { $json = $Body | ConvertTo-Json -Depth 16 -Compress }
    [IO.File]::WriteAllText($file, $json, $utf8)
    $curlArgs += @("--data-binary", "@$file")
  }
  $curlArgs += "$Base$Path"
  $raw = (& curl.exe @curlArgs) -join ""
  if ([string]::IsNullOrWhiteSpace($raw)) { throw "接口无响应: $Method $Path" }
  $obj = $raw | ConvertFrom-Json
  if ($obj -and $obj.PSObject.Properties.Name -contains "error" -and $obj.error) {
    throw "接口返回错误: $Method $Path → $($obj.error)"
  }
  return $obj
}

Write-Host "== 1/6 健康检查 $Base" -ForegroundColor Cyan
$health = Invoke-Api -Method GET -Path "/api/health" -Body $null
Write-Host ("   服务端 v{0}，录制 {1} 条，运行 {2} 次，内置浏览器：{3}" -f $health.version, $health.recordings, $health.runs, $(if ($health.browser_running) { "运行中" } else { "未启动（执行时会自动拉起）" }))

Write-Host "== 2/6 创建录制" -ForegroundColor Cyan
$rec = Invoke-Api -Method POST -Path "/api/recordings" -Body @{
  name        = "示例：新建服务商品"
  description = "smoke 脚本自动创建：打开示例后台 → 新建商品 → 填表 → 保存 → 断言成功"
  start_url   = "$Base/demo/"
  tags        = @("示例", "商品", "smoke")
}
Write-Host ("   recording_id = {0}" -f $rec.id)

$steps = @(
  @{ action = "navigate"; annotation = "打开示例后台首页"; value = "$Base/demo/"; locator = @{ strategy = "css"; value = "html" } },
  @{
    # 故意给一个失效的 uid：验证「自愈定位」能否靠元素指纹救回来
    action = "click"; annotation = "点击【新建商品】按钮（故意用失效 uid，验证自愈）"
    locator = @{ strategy = "uid"; value = "ut_deadbeef" }
    target  = @{ tag = "button"; type = "button"; text = "新建商品"; page = "/demo/" }
  },
  @{
    action = "input"; annotation = "在【商品名称】输入内容"; data_key = "product_name"; value = "{{product_name}}"
    locator = @{ strategy = "near"; value = "商品名称>>input:not([type=hidden])"; fallbacks = @(@{ strategy = "label"; value = "商品名称" }, @{ strategy = "css"; value = "#product-name" }) }
    target  = @{ tag = "input"; type = "text"; label = "商品名称"; placeholder = "请输入商品名称"; page = "/demo/"; field_kind = "text" }
  },
  @{
    action = "input"; annotation = "在【价格】输入内容"; data_key = "price"; value = "{{price}}"
    locator = @{ strategy = "near"; value = "价格>>input:not([type=hidden])"; fallbacks = @(@{ strategy = "label"; value = "价格" }, @{ strategy = "css"; value = "#product-price" }) }
    target  = @{ tag = "input"; type = "number"; label = "价格"; page = "/demo/"; field_kind = "price" }
  },
  @{
    action = "input"; annotation = "在【库存】输入内容"; data_key = "stock"; value = "{{stock}}"
    locator = @{ strategy = "near"; value = "库存>>input:not([type=hidden])"; fallbacks = @(@{ strategy = "label"; value = "库存" }, @{ strategy = "css"; value = "#product-stock" }) }
    target  = @{ tag = "input"; type = "number"; label = "库存"; page = "/demo/"; field_kind = "number" }
  },
  @{
    action = "select"; annotation = "在【商品分类】选择「服务类」"; data_key = "category"; value = "{{category}}"
    locator = @{ strategy = "near"; value = "商品分类>>select"; fallbacks = @(@{ strategy = "label"; value = "商品分类" }, @{ strategy = "css"; value = "#product-category" }) }
    target  = @{ tag = "select"; label = "商品分类"; page = "/demo/"; field_kind = "select"; options = @("服务类", "实物类", "虚拟类") }
  },
  @{
    action = "input"; annotation = "在【商品描述】输入内容"; data_key = "description"; value = "{{description}}"
    locator = @{ strategy = "near"; value = "商品描述>>textarea"; fallbacks = @(@{ strategy = "label"; value = "商品描述" }, @{ strategy = "css"; value = "#product-desc" }) }
    target  = @{ tag = "textarea"; label = "商品描述"; placeholder = "请输入商品描述"; page = "/demo/"; field_kind = "textarea" }
  },
  @{
    action = "upload"; annotation = "上传封面图片"; data_key = "cover"; value = "{{cover}}"
    locator = @{ strategy = "css"; value = "#product-cover" }
    target  = @{ tag = "input"; type = "file"; label = "封面图片"; page = "/demo/"; field_kind = "file" }
  },
  @{
    action = "click"; annotation = "点击【保存商品】按钮"
    locator = @{ strategy = "role"; value = "button|保存商品"; fallbacks = @(@{ strategy = "text"; value = "保存商品" }, @{ strategy = "css"; value = "#btn-save-product" }) }
    target  = @{ tag = "button"; type = "submit"; text = "保存商品"; page = "/demo/" }
  },
  @{
    action = "assert"; annotation = "断言保存成功（页面标题带上创建成功）"; value = "js:document.title.indexOf('商品创建成功')>=0"
    locator = @{ strategy = "css"; value = "body" }
    target  = @{ tag = "body"; page = "/demo/" }
  }
)

Write-Host ("== 3/6 写入 {0} 个步骤" -f $steps.Count) -ForegroundColor Cyan
foreach ($s in $steps) { $null = Invoke-Api -Method POST -Path "/api/recordings/$($rec.id)/steps" -Body $s }
$rec = Invoke-Api -Method GET -Path "/api/recordings/$($rec.id)" -Body $null
Write-Host ("   录制名「{0}」，共 {1} 步" -f $rec.name, $rec.step_count)

Write-Host "== 4/6 登记元素库（模拟插件上报 data-uitest-id 绑定）" -ForegroundColor Cyan
foreach ($e in @(
    @{ page = "/demo/"; key = "ut_smoke_name"; selector = "#product-name"; tag = "input"; type = "text"; label = "商品名称"; descriptor = "商品名称 输入框" },
    @{ page = "/demo/"; key = "ut_smoke_save"; selector = "#btn-save-product"; tag = "button"; type = "submit"; label = "保存商品"; descriptor = "保存商品 按钮" }
  )) { $null = Invoke-Api -Method POST -Path "/api/elements/bind" -Body $e }
$elems = Invoke-Api -Method GET -Path "/api/elements" -Body $null
Write-Host ("   元素库共 {0} 条" -f $elems.items.Count)

Write-Host "== 5/6 AI / 规则生成测试数据" -ForegroundColor Cyan
$ds = Invoke-Api -Method POST -Path "/api/ai/dataset" -Body @{
  recording_id = $rec.id
  instruction  = "商品名称带时间戳，价格 99-299，库存 50，分类选服务类"
}
Write-Host ("   数据来源：{0}" -f $(if ($ds.source -eq "ai") { "大模型" } else { "内置规则" }))
if ($ds.warning) { Write-Host ("   提示：" + $ds.warning) -ForegroundColor Yellow }
foreach ($f in $ds.fields) {
  $val = $ds.dataset.($f.key)
  Write-Host ("   - {0,-14} {1,-10} = {2}" -f $f.key, $f.label, $val)
}

Write-Host "== 6/6 用内置浏览器执行" -ForegroundColor Cyan
$run = Invoke-Api -Method POST -Path "/api/runs" -Body @{
  recording_id   = $rec.id
  dataset        = $ds.dataset
  dataset_source = $ds.source
  dataset_fields = $ds.fields
  ai             = @{ enabled = $false; instruction = "" }
}
Write-Host ("   run_id = {0}，正在执行…" -f $run.id)

$deadline = (Get-Date).AddSeconds(180)
$last = 0
while ((Get-Date) -lt $deadline) {
  Start-Sleep -Milliseconds 1200
  $run = Invoke-Api -Method GET -Path "/api/runs/$($run.id)" -Body $null
  if ($run.logs.Count -gt $last) {
    for ($i = $last; $i -lt $run.logs.Count; $i++) {
      $l = $run.logs[$i]
      $color = switch ($l.level) { "error" { "Red" } "warn" { "Yellow" } default { "Gray" } }
      Write-Host ("   [{0}] {1}" -f $l.level, $l.message) -ForegroundColor $color
    }
    $last = $run.logs.Count
  }
  if ($run.status -in @("passed", "failed", "canceled")) { break }
}

Write-Host ""
Write-Host ("执行结果：{0}" -f $run.status) -ForegroundColor $(if ($run.status -eq "passed") { "Green" } else { "Red" })
if ($run.error) { Write-Host ("  错误：" + $run.error) -ForegroundColor Red }
Write-Host "逐步结果："
foreach ($sr in $run.step_results) {
  $color = switch ($sr.status) { "passed" { "Green" } "failed" { "Red" } default { "Gray" } }
  $heal = if ($sr.healed) { " [自愈]" } else { "" }
  Write-Host ("   #{0,-2} {1,-6} {2,-34} {3,-8}{4} {5}ms {6}" -f $sr.index, $sr.action, $sr.annotation, $sr.status, $heal, $sr.duration_ms, $sr.message) -ForegroundColor $color
}

if ($run.healables -gt 0) {
  Write-Host ""
  Write-Host "把自愈得到的定位回写进录制…" -ForegroundColor Cyan
  $healed = Invoke-Api -Method POST -Path "/api/runs/$($run.id)/heal" -Body @{}
  Write-Host ("   已更新 {0} 个步骤的定位" -f $healed.updated)
  foreach ($d in $healed.steps) { Write-Host ("   - 第{0}步：{1} → {2}" -f $d.index, $d.from, $d.to) }
}

Write-Host ""
Write-Host "控制台：$Base/#/runs/$($run.id)" -ForegroundColor Cyan
if ($run.status -ne "passed") {
  Write-Host "自检未通过：请查看上面的失败步骤与控制台里的失败截图。" -ForegroundColor Red
  if (-not $Keep) { exit 1 }
  exit 1
}
Write-Host "自检通过 ✅" -ForegroundColor Green
