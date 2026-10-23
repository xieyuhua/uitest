# 停止 uitest 服务端（只结束本项目进程）
Get-Process uitest-server -ErrorAction SilentlyContinue | ForEach-Object {
  Write-Host "停止 uitest-server (PID $($_.Id))" -ForegroundColor Yellow
  Stop-Process -Id $_.Id -Force
}
Get-CimInstance Win32_Process -Filter "Name='msedge.exe'" -ErrorAction SilentlyContinue |
  Where-Object { $_.CommandLine -match 'uitest\\data\\browser-profile' } |
  ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Write-Host "已停止" -ForegroundColor Green