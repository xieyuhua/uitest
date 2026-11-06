<#
  接口采集「会话参数」端到端自测（登录后刷新 → token 覆盖请求头）

  它验证的是一条完整链路，全程真跑内置浏览器，不碰正式数据：

    1. demo 登录接口基线：不登录 401，带 token 200
    2. 现场建一条「登录流程录制」（打开登录页 → 填账号密码 → 点登录 → 等跳转）
    3. 建任务：接口自带一个「录制时的旧请求头」，公共参数启用「登录后刷新」
    4. 调 /common/refresh {run_prelogin:true}：先跑前置登录，再把浏览器里的新 token /
       Cookie 写进公共参数，并自动生效参数绑定
    5. 再登一次 → 断言 token 真的变了（不是缓存里的旧值）
    6. 试抓预览：断言真正发出去的 Authorization 就是刷新后的新 token（脱敏后比对），
       且参数绑定压过了接口自己的请求头
    7. 粘贴导入 Cookie / Authorization → 断言落成公共 Cookie + 参数 + 绑定
    8. 没启用「登录后刷新」时只提示、不改参数

  顺带守住一个回归：录制里「点登录 → 等待 2500ms」的等待步骤现在在 Go 侧睡，
  登录跳转把页面 JS 上下文带走也不会再报 Inspected target navigated or closed。

  用法：
    powershell -ExecutionPolicy Bypass -File scripts\collect-session-smoke.ps1
    powershell -ExecutionPolicy Bypass -File scripts\collect-session-smoke.ps1 -Port 18492
    powershell -ExecutionPolicy Bypass -File scripts\collect-session-smoke.ps1 -Keep    # 跑完不关服务
    powershell -ExecutionPolicy Bypass -File scripts\collect-session-smoke.ps1 -NoBuild # 复用上次编译的 exe
#>
param(
  [int]$Port = 18492,
  [switch]$Keep,
  [switch]$NoBuild
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root
$base = "http://127.0.0.1:$Port"
$work = Join-Path $env:TEMP ("uitest-session-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
New-Item -ItemType Directory -Force -Path $work | Out-Null
$exe = Join-Path $work "uitest-server.exe"

$results = @()
$proc = $null

function Report([string]$name, [bool]$ok, [string]$detail) {
  $script:results += [pscustomobject]@{ Name = $name; OK = $ok; Detail = $detail }
  if ($ok) {
    Write-Host ("  [PASS] " + $name + "  " + $detail) -ForegroundColor Green
  } else {
    Write-Host ("  [FAIL] " + $name + "  " + $detail) -ForegroundColor Red
  }
}

function Try-Get([string]$url) {
  try {
    return @{ ok = $true; status = 200; body = (Invoke-RestMethod -Uri $url -Method Get -TimeoutSec 60) }
  } catch {
    $code = 0
    if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
    return @{ ok = $false; status = $code; body = $null }
  }
}

function Post-Json([string]$path, $body, [int]$timeoutSec = 120) {
  $json = "{}"
  if ($null -ne $body) {
    if ($body -is [string]) { $json = $body } else { $json = $body | ConvertTo-Json -Depth 12 }
  }
  return Invoke-RestMethod -Uri ($base + $path) -Method Post -Body $json -ContentType "application/json" -TimeoutSec $timeoutSec
}

function Has-Item($list, [string]$want) {
  foreach ($x in @($list)) { if ([string]$x -eq $want) { return $true } }
  return $false
}

function Join-List($list) {
  return (@($list) | ForEach-Object { [string]$_ }) -join "、"
}

function Count-Real($list) {
  $n = 0
  foreach ($x in @($list)) { if ($null -ne $x -and ("$x" -ne "")) { $n++ } }
  return $n
}

try {
  # ---------------------------------------------------------------- 1. 编译 / 启动
  Write-Host ""
  Write-Host "== 1/6 编译 ==" -ForegroundColor Cyan
  if (-not $NoBuild) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "没找到 go 命令，请先装 Go 并加进 PATH" }
    & go build -o $exe .
    if ($LASTEXITCODE -ne 0) { throw "go build 失败（看上面的编译错误）" }
    Report "编译" $true "go build 通过"
  } else {
    $exe = Join-Path $root "uitest-server.exe"
    if (-not (Test-Path $exe)) { throw "没找到 $exe（先不带 -NoBuild 跑一次）" }
    Report "编译" $true "复用已有 $exe"
  }

  Write-Host ""
  Write-Host "== 2/6 启动测试实例（端口 $Port，数据目录 $work\data）==" -ForegroundColor Cyan
  $env:UITEST_DATA_DIR = Join-Path $work "data"
  $proc = Start-Process -FilePath $exe -ArgumentList @("-port", "$Port", "-host", "127.0.0.1") -PassThru -WindowStyle Hidden
  $ready = $false
  for ($i = 0; $i -lt 80; $i++) {
    Start-Sleep -Milliseconds 250
    $probe = Try-Get "$base/api/collect/options"
    if ($probe.ok) { $ready = $true; break }
    if ($proc.HasExited) { break }
  }
  if (-not $ready) { throw "服务没起来（端口 $Port 可能被占用，换 -Port）" }
  Report "启动服务" $true "$base 已就绪"

  # ---------------------------------------------------------------- 3. demo 登录基线
  Write-Host ""
  Write-Host "== 3/6 demo 登录基线 ==" -ForegroundColor Cyan
  $login = Post-Json "/demo/api/login" @{ username = "admin"; password = "123456" }
  $manualToken = [string]$login.data
  Report "登录接口返回 token" ($login.code -eq 0 -and $manualToken.StartsWith("demo-token-")) `
    ("token=" + $manualToken.Substring(0, [Math]::Min(16, $manualToken.Length)) + "…")

  $unauth = Try-Get "$base/demo/api/orders?page=1&size=5"
  Report "未登录访问订单接口应为 401" ($unauth.status -eq 401) ("实际 HTTP " + $unauth.status)

  $authResp = $null
  try {
    $authResp = Invoke-RestMethod -Uri "$base/demo/api/orders?page=1&size=5" -Method Get `
      -Headers @{ Authorization = "Bearer $manualToken"; "X-Tenant" = "demo" } -TimeoutSec 30
    Report "带 token 访问订单接口" ($authResp.data.total -eq 42) ("total=" + $authResp.data.total)
  } catch {
    Report "带 token 访问订单接口" $false $_.Exception.Message
  }

  # ---------------------------------------------------------------- 4. 建登录流程录制
  Write-Host ""
  Write-Host "== 4/6 建「登录流程录制」（内置浏览器真登录一次）==" -ForegroundColor Cyan
  $recName = "selftest-login-$Port"
  $rec = Post-Json "/api/recordings" @{
    name        = $recName
    description = "自测用：登录 demo 站点，token 写进 localStorage"
    start_url   = "$base/demo/collect-login.html"
    tags        = @("selftest")
  }
  $rid = [string]$rec.id
  Report "建录制" ($rid -ne "") ("id=" + $rid)

  $steps = @(
    @{ action = "navigate"; value = "$base/demo/collect-login.html"; annotation = "打开登录页" },
    @{ action = "input"; locator = @{ strategy = "id"; value = "username" }; value = "admin"; annotation = "填账号" },
    @{ action = "input"; locator = @{ strategy = "id"; value = "password" }; value = "123456"; annotation = "填密码" },
    @{ action = "click"; locator = @{ strategy = "id"; value = "submit" }; annotation = "点登录" },
    @{ action = "wait"; value = "2500"; annotation = "等跳转到订单列表页" }
  )
  foreach ($st in $steps) { $null = Post-Json "/api/recordings/$rid/steps" $st }
  $recFull = Try-Get "$base/api/recordings/$rid"
  $stepCount = 0
  if ($recFull.ok) { $stepCount = @($recFull.body.steps).Count }
  Report "交步骤（导航 / 输入 / 点击 / 等待）" ($stepCount -eq 5) ("步骤=" + $stepCount)

  # ---------------------------------------------------------------- 5. 登录后刷新 + 试抓
  Write-Host ""
  Write-Host "== 5/6 登录后刷新公共参数 + 试抓（会真发请求）==" -ForegroundColor Cyan
  $taskBody = @{
    name      = "selftest-session-$Port"
    start_url = "$base/demo/collect-orders.html"
    endpoints = @(@{
        id         = "ep-orders"
        name       = "订单列表"
        method     = "GET"
        enabled    = $true
        url        = "$base/demo/api/orders?page=1&size=5"
        headers    = @{ "X-Tenant" = "recorded-tenant" }
        pagination = @{ mode = "none" }
      })
    session   = @{ mode = "browser"; host = $base }
    prelogin  = @{
      enabled        = $true
      recording_id   = $rid
      recording_name = $recName
      mode           = "always"
      timeout_seconds = 150
    }
    common    = @{
      params   = @{ tenant = "common-tenant" }
      bindings = @(
        @{ param = "token"; target = "header"; name = "Authorization"; template = "Bearer {{token}}" },
        @{ param = "tenant"; target = "header"; name = "X-Tenant"; template = "{{tenant}}" }
      )
      refresh  = @{
        enabled      = $true
        token_param  = "token"
        token_header = "Authorization"
        cookies      = @{ demo_sid = "sid" }
        storage      = @{ "localStorage:demo_token" = "token" }
      }
    }
  }
  $task = Post-Json "/api/collect/tasks" $taskBody 60
  $tid = [string]$task.id
  Report "建采集任务（带前置登录 + 登录后刷新）" ($tid -ne "") ("id=" + $tid + " 接口=" + $task.endpoint_count)

  $rf1 = Post-Json "/api/collect/tasks/$tid/common/refresh" @{ run_prelogin = $true } 300
  $rf1Info = $rf1.refresh
  Report "刷新接口返回 ok" ($rf1.ok -eq $true) ("ok=" + $rf1.ok + " err=" + $rf1.error)
  Report "前置登录执行成功" ($rf1.prelogin_ok -eq $true) ("prelogin_ok=" + $rf1.prelogin_ok + " err=" + $rf1.prelogin_error)
  Report "刷新写入了 token 参数" (Has-Item $rf1Info.applied "参数:token") ("applied=" + (Join-List $rf1Info.applied))
  Report "Cookie 映射成公共参数（demo_sid → sid）" (Has-Item $rf1Info.applied "参数:sid") ("applied=" + (Join-List $rf1Info.applied))
  $tokA = [string]$rf1Info.params.token
  $sidA = [string]$rf1Info.params.sid
  Report "token 已脱敏（不是明文）" ($tokA -like "demo*" -and $tokA.Contains("…")) ("token=" + $tokA)
  Report "Cookie 已脱敏" ($sidA -ne "" -and $sidA.Contains("…")) ("sid=" + $sidA + " cookie=" + $rf1Info.cookies.demo_sid)

  $bindAuth = $false
  foreach ($b in @($rf1Info.bindings)) {
    if ([string]$b.target -eq "header" -and ([string]$b.name).ToLower() -eq "authorization" -and [string]$b.param -eq "token") { $bindAuth = $true }
  }
  Report "生成了 token → header:Authorization 绑定" $bindAuth ("bindings=" + @($rf1Info.bindings).Count + " 条")

  # 再登录一次：token 必须跟着变（证明取的是浏览器里的最新值，不是录制/缓存里的旧值）
  $rf2 = Post-Json "/api/collect/tasks/$tid/common/refresh" @{ run_prelogin = $true } 300
  $tokB = [string]$rf2.refresh.params.token
  Report "再次登录后 token 会变（用的是最新值）" ($tokA -ne "" -and $tokB -ne "" -and $tokA -ne $tokB) ("上次=" + $tokA + " 本次=" + $tokB)

  $pv = Post-Json "/api/collect/tasks/$tid/preview" @{ limit = 1 } 180
  $item = @($pv.items)[0]
  if ($null -eq $item) {
    Report "试抓预览" $false "没有返回结果"
  } else {
    Report "试抓返回 200（登录态有效）" ($item.status -eq 200) ("status=" + $item.status + " err=" + $item.error)
    $hdrs = $item.request.headers
    $authHdr = [string]$hdrs.Authorization
    $expectAuth = "Bearer " + $tokB
    Report "真正发出去的 Authorization = 刷新后的新 token" ($authHdr -eq $expectAuth) ("实际=" + $authHdr + " 期望=" + $expectAuth)
    $tenantHdr = [string]$hdrs."X-Tenant"
    Report "参数绑定压过接口自己的请求头" ($tenantHdr -eq "common-tenant") ("X-Tenant=" + $tenantHdr + "（接口里写的是 recorded-tenant）")
    $procText = (Join-List $item.process)
    Report "试抓过程里有「登录后刷新」步骤" ($procText.Contains("登录后刷新公共参数")) ("过程=" + $procText)
    $pvTok = [string]$item.request.vars.token
    Report "试抓变量里的 token 与刷新结果一致" ($pvTok -eq $tokB) ("vars.token=" + $pvTok)
    $rows = @($item.records).Count
    if ($rows -eq 0) { $rows = @($item.preview).Count }
    Report "试抓解析出数据" ($rows -ge 1 -or $item.records_path -ne "") ("记录数=" + $rows + " 路径=" + $item.records_path)
  }

  # ---------------------------------------------------------------- 6. 导入 / 未启用
  Write-Host ""
  Write-Host "== 6/6 粘贴导入 + 未启用刷新时的行为 ==" -ForegroundColor Cyan
  $importText = "Cookie: SESSION=abc123; uid=42" + "`n" + "X-Tenant: demo" + "`n" + "Authorization: Bearer abcdef1234567890"
  $imp = Post-Json "/api/collect/tasks/$tid/common/import" @{ headers = $importText } 60
  $c = $imp.common
  Report "导入 Cookie 落成公共 Cookie" ([string]$c.cookies.SESSION -eq "abc123") ("SESSION=" + $c.cookies.SESSION)
  Report "导入 Authorization 落成 token 参数" ([string]$c.params.token -eq "abcdef1234567890") ("token=" + $c.params.token)
  $impBind = $false
  foreach ($b in @($c.bindings)) {
    if ([string]$b.param -eq "token" -and ([string]$b.name).ToLower() -eq "authorization") { $impBind = $true }
  }
  Report "导入鉴权头自动生成绑定" $impBind ("bindings=" + @($c.bindings).Count + " 条")
  Report "导入普通请求头进公共请求头" ([string]$c.headers."X-Tenant" -eq "demo") ("X-Tenant=" + $c.headers."X-Tenant")
  $notes = Join-List $imp.notes
  Report "导入返回说明" ($notes -ne "") ("notes=" + $notes)

  $task2Body = @{
    name      = "selftest-session-off-$Port"
    start_url = "$base/demo/collect-orders.html"
    endpoints = @(@{
        id = "ep-orders"; name = "订单列表"; method = "GET"; enabled = $true
        url = "$base/demo/api/orders?page=1&size=5"
      })
    common    = @{
      bindings = @(@{ param = "token"; target = "header"; name = "Authorization"; template = "Bearer {{token}}" })
      params   = @{ token = "stale-token" }
      refresh  = @{ enabled = $false }
    }
  }
  $task2 = Post-Json "/api/collect/tasks" $task2Body 60
  $tid2 = [string]$task2.id
  $rf3 = Post-Json "/api/collect/tasks/$tid2/common/refresh" @{} 120
  $rf3Info = $rf3.refresh
  $rf3Notes = Join-List $rf3Info.notes
  Report "没启用刷新时不改参数" ($rf3.ok -eq $true -and (Count-Real $rf3Info.applied) -eq 0) ("applied=" + (Count-Real $rf3Info.applied) + " notes=" + $rf3Notes)
  Report "没启用刷新时给出提示" ($rf3Notes.Contains("没启用")) ("notes=" + $rf3Notes)
  $staleShown = [string]$rf3Info.params.token
  Report "旧 token 原样保留（只是脱敏展示）" ($staleShown.StartsWith("st") -and $staleShown.EndsWith("en") -and $staleShown.Contains("…")) ("token=" + $staleShown)

} catch {
  Write-Host ("测试中断：" + $_.Exception.Message) -ForegroundColor Red
} finally {
  if (-not $Keep -and $proc -ne $null -and -not $proc.HasExited) {
    try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch { }
  }
  Remove-Item Env:\UITEST_DATA_DIR -ErrorAction SilentlyContinue
}

# ---------------------------------------------------------------- 汇总
Write-Host ""
Write-Host "==================== 汇总 ====================" -ForegroundColor Cyan
$pass = @($results | Where-Object { $_.OK }).Count
$total = $results.Count
foreach ($r in $results) {
  $mark = "FAIL"
  if ($r.OK) { $mark = "PASS" }
  $color = "Red"
  if ($r.OK) { $color = "Green" }
  Write-Host ("  " + $mark + "  " + $r.Name + "  " + $r.Detail) -ForegroundColor $color
}
Write-Host ""
Write-Host ("  通过 " + $pass + " / " + $total) -ForegroundColor Cyan

if ($Keep -and $proc -ne $null -and -not $proc.HasExited) {
  Write-Host ""
  Write-Host ("服务仍在运行：" + $base + "（数据目录 " + $work + "）") -ForegroundColor Yellow
  Write-Host "手工看：/demo/collect-login.html 登录 → 采集任务 → 详情 → 公共参数 → 登录后刷新" -ForegroundColor Yellow
  Write-Host ("关掉它：Stop-Process -Id " + $proc.Id + " -Force") -ForegroundColor Yellow
} else {
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

if ($pass -eq $total -and $total -ge 15) {
  Write-Host "全部通过 ✔" -ForegroundColor Green
  exit 0
} else {
  Write-Host "有失败项：把上面 FAIL 那几行发我即可定位。" -ForegroundColor Red
  exit 1
}