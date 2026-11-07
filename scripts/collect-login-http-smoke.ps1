<#
  接口采集「登录接口换 token」端到端自测（不碰浏览器）

  验证链路（全程真请求本地 demo 服务，不碰正式数据）：

    1. demo 登录接口基线：密码错 401；密码对返回 {"code":0,"data":"demo-token-…"} + Set-Cookie demo_sid
    2. 建任务：会话来源 = 登录接口（source=http），接口里故意留着录制时的旧 Authorization
    3. 测试登录 /login/test：断言 token 从 data 取出、写进参数 token、绑到 Authorization、收下 Set-Cookie，
       并且请求体里的密码在返回过程里已脱敏
    4. 刷新会话 /common/refresh：断言 token / Cookie 被写回任务「公共参数」（下次打开任务就是最新值）
    5. 再刷一次：token 必须变（每次登录都换新的，证明不是缓存旧值）
    6. 试抓：断言真正发出去的 Authorization = 本次登录换到的 token（脱敏比对），
       接口里录制的旧 token 没被发出去；时间线里有「调登录接口换 token」步骤且状态 ok
    7. 密码错误时：/login/test 返回 ok=false、trace.status=401、error 非空

  用法：
    powershell -ExecutionPolicy Bypass -File scripts\collect-login-http-smoke.ps1
    powershell -ExecutionPolicy Bypass -File scripts\collect-login-http-smoke.ps1 -Port 18493
    powershell -ExecutionPolicy Bypass -File scripts\collect-login-http-smoke.ps1 -Keep
    powershell -ExecutionPolicy Bypass -File scripts\collect-login-http-smoke.ps1 -NoBuild
#>
param(
  [int]$Port = 18493,
  [switch]$Keep,
  [switch]$NoBuild
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.Encoding]::UTF8

$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root
$base = "http://127.0.0.1:$Port"
$work = Join-Path $env:TEMP ("uitest-login-http-" + [Guid]::NewGuid().ToString("N").Substring(0, 8))
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

# 直接 POST 到 demo 登录接口：返回状态码 + 原始响应（401 不抛异常）。
# 不用连接复用：401 之后偶发连接被服务端提前关闭，会表现为「拿不到响应」，这里断开重试。
function Post-DemoLogin([string]$password) {
  $json = (@{ username = "demo"; password = $password } | ConvertTo-Json -Compress)
  $last = @{ status = 0; token = ""; cookie = "" }
  for ($i = 0; $i -lt 3; $i++) {
    try {
      $r = Invoke-WebRequest -Uri "$base/demo/api/login" -Method Post -Body $json `
        -ContentType "application/json" -TimeoutSec 20 -UseBasicParsing -DisableKeepAlive
      $token = ""
      try { $token = [string]($r.Content | ConvertFrom-Json).data } catch { }
      return @{ status = [int]$r.StatusCode; token = $token; cookie = [string]$r.Headers["Set-Cookie"] }
    } catch {
      $code = 0
      if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
      $last = @{ status = $code; token = ""; cookie = "" }
      if ($code -ne 0) { return $last }   # 4xx/5xx 是确定的答案，不用重试
      Start-Sleep -Milliseconds 400
    }
  }
  return $last
}

function Has-Item($list, [string]$want) {
  foreach ($x in @($list)) { if ([string]$x -eq $want) { return $true } }
  return $false
}

function Join-List($list) {
  return (@($list) | ForEach-Object { [string]$_ }) -join "、"
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

  # ---------------------------------------------------------------- 2. 登录接口基线
  Write-Host ""
  Write-Host "== 3/6 登录接口基线（密码错 / 密码对）==" -ForegroundColor Cyan
  $bad = Post-DemoLogin "wrong"
  Report "密码错误时登录接口返回 401" ($bad.status -eq 401) ("status=" + $bad.status)

  $good = Post-DemoLogin "demo123"
  Report "密码正确时返回 token" ($good.status -eq 200 -and $good.token.StartsWith("demo-token-")) ("status=" + $good.status + " token=" + $good.token.Substring(0, [Math]::Min(18, $good.token.Length)) + "…")
  Report "登录接口种下 demo_sid Cookie" ($good.cookie.Contains("demo_sid=")) ("Set-Cookie=" + $good.cookie)

  # ---------------------------------------------------------------- 3. 建任务
  Write-Host ""
  Write-Host "== 4/6 建任务：会话来源 = 登录接口 ==" -ForegroundColor Cyan
  $loginCfg = @{
    url        = "$base/demo/api/login"
    method     = "POST"
    body       = '{"username":"{{username}}","password":"{{password}}"}'
    vars       = @{ username = "demo"; password = "demo123" }
    token_path = "data"
    with_cookies = $true
  }
  $taskBody = @{
    name      = "selftest-login-http-$Port"
    start_url = "$base/demo/collect-orders.html"
    endpoints = @(@{
        id         = "ep-orders"
        name       = "订单列表"
        method     = "GET"
        enabled    = $true
        url        = "$base/demo/api/orders?page=1&size=5"
        headers    = @{ "Authorization" = "Bearer recorded-stale-token"; "X-Tenant" = "recorded-tenant" }
        pagination = @{ mode = "none" }
      })
    session = @{
      source = "http"
      host   = $base
      token  = @{
        source  = "login"
        param   = "token"
        header  = "Authorization"
        prefix  = "Bearer "
        login   = $loginCfg
      }
    }
    common = @{
      # 先「批量导入」一份旧 Cookie：登录之后要能被刷新成新值
      cookies  = @{ demo_sid = "imported-stale-cookie" }
      params   = @{ tenant = "common-tenant" }
      bindings = @(
        @{ param = "token"; target = "header"; name = "Authorization"; template = "Bearer {{token}}" },
        @{ param = "tenant"; target = "header"; name = "X-Tenant"; template = "{{tenant}}" }
      )
    }
  }
  $task = Post-Json "/api/collect/tasks" $taskBody 60
  $tid = [string]$task.id
  Report "建采集任务（会话来源=登录接口）" ($tid -ne "") ("id=" + $tid + " 接口=" + $task.endpoint_count)
  Report "任务里会话来源是 http" ([string]$task.session.source -eq "http") ("source=" + $task.session.source + " · " + $task.session.source_text)

  # ---------------------------------------------------------------- 4. 测试登录
  Write-Host ""
  Write-Host "== 5/6 测试登录 / 刷新会话 / 试抓 ==" -ForegroundColor Cyan
  $lt = Post-Json "/api/collect/tasks/$tid/login/test" @{} 90
  $tr = $lt.trace
  Report "测试登录 ok" ($lt.ok -eq $true) ("ok=" + $lt.ok + " err=" + $lt.error)
  Report "登录接口返回 200" ([int]$tr.status -eq 200) ("status=" + $tr.status)
  Report "token 取值路径 / 参数名 / 请求头回填正确" ([string]$tr.token_path -eq "data" -and [string]$tr.param -eq "token" -and [string]$tr.header -eq "Authorization") ("token_path=" + $tr.token_path + " param=" + $tr.param + " header=" + $tr.header)
  Report "返回的 token 已脱敏" ([string]$tr.token_preview -ne "" -and -not ([string]$tr.token_preview).Contains("demo-token-")) ("token_preview=" + $tr.token_preview + "（" + $tr.token_length + " 字符）")
  Report "请求体里的密码已脱敏" (([string]$tr.body).Contains("…") -and -not ([string]$tr.body).Contains("demo123")) ("body=" + $tr.body)
  $trCookies = @($tr.cookies)
  Report "拿到登录返回的 demo_sid" (Has-Item $trCookies "demo_sid") ("cookies=" + (Join-List $trCookies))
  Report "测试登录报告生效项" (Has-Item $lt.applied "参数:token") ("applied=" + (Join-List $lt.applied))

  # ---- 刷新会话并写回公共参数
  $rf1 = Post-Json "/api/collect/tasks/$tid/common/refresh" @{} 120
  $rf1Info = $rf1.refresh
  Report "刷新会话 ok" ($rf1.ok -eq $true) ("ok=" + $rf1.ok + " err=" + $rf1.error)
  Report "刷新结果标明来源是登录接口" ([string]$rf1Info.source -eq "http") ("source=" + $rf1Info.source + " · " + $rf1Info.source_text)
  Report "刷新结果带回登录过程" ($null -ne $rf1Info.login -and [int]$rf1Info.login.status -eq 200) ("login.status=" + $rf1Info.login.status + " token=" + $rf1Info.login.token_preview)
  Report "刷新写回参数 token" (Has-Item $rf1.saved "参数:token") ("saved=" + (Join-List $rf1.saved))
  Report "刷新写回已导入的 Cookie demo_sid" (Has-Item $rf1.saved "Cookie:demo_sid") ("saved=" + (Join-List $rf1.saved))

  $after = Try-Get "$base/api/collect/tasks/$tid"
  $storedTok = [string]$after.body.common.params.token
  $storedCookie = [string]$after.body.common.cookies.demo_sid
  Report "任务配置里的 token 已换成登录值" ($storedTok.StartsWith("demo-token-")) ("token=" + $storedTok.Substring(0, [Math]::Min(18, $storedTok.Length)) + "…")
  Report "任务配置里的 demo_sid 已刷新" ($storedCookie.StartsWith("demo-token-")) ("demo_sid=" + $storedCookie.Substring(0, [Math]::Min(18, $storedCookie.Length)) + "…")

  # 再刷一次：登录每次都给新 token，写回的值必须跟着变
  $rf2 = Post-Json "/api/collect/tasks/$tid/common/refresh" @{} 120
  $after2 = Try-Get "$base/api/collect/tasks/$tid"
  $storedTok2 = [string]$after2.body.common.params.token
  Report "再次刷新后 token 变了（不是缓存旧值）" ($storedTok -ne "" -and $storedTok2 -ne "" -and $storedTok -ne $storedTok2) ("上次=" + $storedTok.Substring(0, [Math]::Min(12, $storedTok.Length)) + "… 本次=" + $storedTok2.Substring(0, [Math]::Min(12, $storedTok2.Length)) + "…")

  # ---------------- 试抓
  $pv = Post-Json "/api/collect/tasks/$tid/preview" @{ limit = 1 } 180
  $item = @($pv.items)[0]
  if ($null -eq $item) {
    Report "试抓预览" $false "没有返回结果"
  } else {
    Report "试抓返回 200（登录接口换的 token 有效）" ($item.status -eq 200) ("status=" + $item.status + " err=" + $item.error)
    $hdrs = $item.request.headers
    $authHdr = [string]$hdrs.Authorization
    $pvTok = [string]$item.request.vars.token
    Report "试抓用了登录换到的新 token" ($authHdr -eq ("Bearer " + $pvTok) -and $pvTok.StartsWith("demo")) ("Authorization=" + $authHdr)
    Report "接口里录制的旧 Authorization 没被发出去" (-not $authHdr.Contains("recorded-stale-token")) ("Authorization=" + $authHdr)
    Report "参数绑定压过接口自己的请求头" ([string]$hdrs."X-Tenant" -eq "common-tenant") ("X-Tenant=" + $hdrs."X-Tenant")
    $procText = (Join-List $item.process)
    Report "试抓过程里有登录接口换 token 这一步" ($procText.Contains("登录")) ("process=" + $procText.Substring(0, [Math]::Min(120, $procText.Length)) + "…")

    $tl = @($item.timeline)
    $loginStep = @($tl | Where-Object { [string]$_.action -eq "login" })[0]
    Report "时间线里有「调登录接口换 token」步骤" ($null -ne $loginStep -and [string]$loginStep.status -eq "ok") ("title=" + $loginStep.title + " status=" + $loginStep.status)
    if ($null -ne $loginStep) {
      $det = $loginStep.details
      Report "登录步骤展示 token 路径 / 参数 / 请求头" ([string]$det."token 取值路径" -eq "data" -and [string]$det."写入公共参数" -eq "token" -and ([string]$det."绑定请求头").StartsWith("Authorization")) ("路径=" + $det."token 取值路径" + " 参数=" + $det."写入公共参数" + " 头=" + $det."绑定请求头")
      Report "登录步骤展示了 Set-Cookie" (([string]$det."Set-Cookie").Contains("demo_sid")) ("Set-Cookie=" + $det."Set-Cookie")
    }
    $reqStep = @($tl | Where-Object { [string]$_.action -eq "request" })[0]
    Report "时间线请求步骤的 Authorization 就是本次 token" ([string]$reqStep.details."Header Authorization" -eq $authHdr) ("header=" + $reqStep.details."Header Authorization")
    $leak = $false
    foreach ($st in $tl) {
      if ($null -ne $st.details) {
        foreach ($prop in $st.details.PSObject.Properties) {
          if (([string]$prop.Value).Contains("demo-token-")) { $leak = $true }
        }
      }
      if (([string]$st.value_used).Contains("demo-token-")) { $leak = $true }
    }
    Report "时间线里的 token 全部脱敏" (-not $leak) ("leak=" + $leak)
    $rows = @($item.records).Count
    if ($rows -eq 0) { $rows = @($item.preview).Count }
    Report "试抓解析出数据" ($rows -ge 1) ("记录数=" + $rows + " 路径=" + $item.records_path)
  }

  # ---------------------------------------------------------------- 5. 密码错误
  Write-Host ""
  Write-Host "== 6/6 密码错误时的测试登录 ==" -ForegroundColor Cyan
  $badLogin = @{
    url        = "$base/demo/api/login"
    method     = "POST"
    body       = '{"username":"{{username}}","password":"{{password}}"}'
    vars       = @{ username = "demo"; password = "wrong" }
    token_path = "data"
  }
  $bt = Post-Json "/api/collect/tasks/$tid/login/test" @{ login = $badLogin } 90
  Report "密码错误时测试登录 ok=false" ($bt.ok -eq $false) ("ok=" + $bt.ok)
  Report "密码错误时登录状态 401" ([int]$bt.trace.status -eq 401) ("status=" + $bt.trace.status)
  Report "密码错误时给出错误说明" ([string]$bt.error -ne "" -and [string]$bt.trace.error -ne "") ("error=" + $bt.error + " · trace.error=" + $bt.trace.error)

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
