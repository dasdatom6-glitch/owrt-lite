
$ErrorActionPreference = "Stop"
$AdminUrl = "http://127.0.0.1:8080"

# 1. Login
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
Invoke-WebRequest -Uri "$AdminUrl/login" -Method Post -Body '{"password":"admin"}' -WebSession $session -ContentType "application/json" | Out-Null

# 2. Set Global Mode
Write-Host "Setting Proxy Mode to 'global'..."
Invoke-WebRequest -Uri "$AdminUrl/system/proxy" -Method Post -Body '{"mode":"global"}' -WebSession $session -ContentType "application/json" | Out-Null

# 3. Check Registry
Write-Host "Checking Windows Registry..."
Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' | Select-Object ProxyEnable, ProxyServer, ProxyOverride
