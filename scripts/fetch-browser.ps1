# 下载一份独立的浏览器内核（Chrome for Testing）到 .\browser，让服务端不再依赖系统里的 Edge/Chrome。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File scripts\fetch-browser.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\fetch-browser.ps1 -Channel beta
#   powershell -ExecutionPolicy Bypass -File scripts\fetch-browser.ps1 -Version 141.0.7390.65
#   powershell -ExecutionPolicy Bypass -File scripts\fetch-browser.ps1 -Mirror npmmirror   # 国内镜像
#
# 下载完成后目录结构：<项目>\browser\chrome-win64\chrome.exe
# 服务端默认就会去这里找内核（设置里 kernel_dir 留空即可）；也可以在控制台「设置 → 自带内核目录」显式指定。
param(
  [string]$Channel = 'stable',        # stable | beta | dev | canary
  [string]$Version = '',
  [string]$Arch = 'win64',            # win64 | win32 | mac-x64 | mac-arm64 | linux64
  [ValidateSet('official', 'npmmirror')]
  [string]$Mirror = 'official',
  [switch]$Force
)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$dest = Join-Path $root 'browser'
$ProgressPreference = 'SilentlyContinue'

function Fail($msg) {
  Write-Host ""
  Write-Host "× $msg" -ForegroundColor Red
  exit 1
}

Write-Host "[1/4] 解析版本…" -ForegroundColor Cyan
if (-not $Version) {
  $metaUrls = @(
    'https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json',
    'https://cdn.npmmirror.com/binaries/chrome-for-testing/last-known-good-versions-with-downloads.json'
  )
  $meta = $null
  foreach ($u in $metaUrls) {
    try {
      $meta = Invoke-RestMethod -Uri $u -TimeoutSec 12 -UseBasicParsing
      Write-Host "    版本清单来源：$u"
      break
    } catch {
      Write-Host "    取版本清单失败：$u" -ForegroundColor DarkYellow
    }
  }
  if (-not $meta) {
    Fail "无法获取版本清单（网络不通？）。可以：① 换 -Mirror npmmirror；② 手工下载 chrome-<arch>.zip 解压到 $dest；③ 继续用系统 Edge/Chrome（设置里 kernel_dir 留空）。"
  }
  $key = (Get-Culture).TextInfo.ToTitleCase($Channel)
  $Version = $meta.channels.$key.version
  if (-not $Version) {
    $Version = $meta.channels.$Channel.version
  }
  if (-not $Version) { Fail "版本清单里没有通道 $Channel" }
}
Write-Host "    版本：$Version"

$file = "chrome-$Arch.zip"
$urls = @()
if ($Mirror -eq 'npmmirror') {
  $urls += "https://cdn.npmmirror.com/binaries/chrome-for-testing/$Version/$Arch/$file"
}
$urls += "https://storage.googleapis.com/chrome-for-testing-public/$Version/$Arch/$file"

Write-Host "[2/4] 下载内核（$Arch）…" -ForegroundColor Cyan
if (-not (Test-Path $dest)) { New-Item -ItemType Directory -Path $dest | Out-Null }
$zip = Join-Path $dest $file
if ((Test-Path $zip) -and -not $Force) {
  Write-Host "    已有压缩包，跳过下载：$zip（要重下加 -Force）"
} else {
  $ok = $false
  foreach ($u in $urls) {
    try {
      Write-Host "    下载：$u"
      Invoke-WebRequest -Uri $u -OutFile $zip -TimeoutSec 180 -UseBasicParsing
      $ok = $true
      break
    } catch {
      Write-Host "    失败：$($_.Exception.Message)" -ForegroundColor DarkYellow
    }
  }
  if (-not $ok) {
    Fail "内核下载失败。可手工把 $file 放到 $dest 后重跑；或继续用系统 Edge/Chrome。"
  }
}

Write-Host "[3/4] 解压…" -ForegroundColor Cyan
Add-Type -AssemblyName System.IO.Compression.FileSystem
$target = Join-Path $dest "chrome-$Arch"
if (Test-Path $target) {
  if ($Force) { Remove-Item $target -Recurse -Force } else { Write-Host "    已存在，跳过（要覆盖加 -Force）：$target" }
}
if (-not (Test-Path $target)) {
  [IO.Compression.ZipFile]::ExtractToDirectory($zip, $dest)
}

Write-Host "[4/4] 校验…" -ForegroundColor Cyan
$exe = Join-Path $target 'chrome.exe'
if ($Arch -like 'mac*') { $exe = Join-Path $target 'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing' }
if ($Arch -eq 'linux64') { $exe = Join-Path $target 'chrome' }
if (-not (Test-Path $exe)) { Fail "解压后没找到可执行文件：$exe" }

$ver = (Get-Item $exe).VersionInfo.ProductVersion
Write-Host ""
Write-Host "完成 ✅" -ForegroundColor Green
Write-Host "  内核：$exe"
Write-Host "  版本：$ver"
Write-Host ""
Write-Host "接下来：" -ForegroundColor Cyan
Write-Host "  1. 重启服务端（或直接在控制台「设置 → 自带内核目录」填：$target）"
Write-Host "  2. 控制台点「启动浏览器」→「诊断浏览器」确认渲染正常"
Write-Host "  提示：自带内核是全新用户目录（data\browser-profile），不带你系统浏览器的登录态/插件。"
