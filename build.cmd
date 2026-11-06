@echo off
REM 编译到 dist\ 的入口（cmd / 双击都能用；绕过 PowerShell 执行策略）
REM
REM   build.cmd                 当前平台
REM   build.cmd -All            三个平台
REM   build.cmd -Clean          先清空 dist\ 再编
REM
REM 实际逻辑在 scripts\build.ps1
setlocal
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\build.ps1" %*
set "CODE=%ERRORLEVEL%"
endlocal & exit /b %CODE%
