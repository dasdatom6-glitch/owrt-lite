$ErrorActionPreference = "Stop"

function Get-ProxySettings {
    Get-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings" | Select-Object ProxyServer, ProxyEnable
}

Write-Output "Initial Settings:"
Get-ProxySettings

# Login
$loginUrl = "http://127.0.0.1:8080/login"
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
Invoke-WebRequest -Uri $loginUrl -Method Post -Body '{"password":"admin"}' -WebSession $session -ContentType "application/json" | Out-Null

# Set Global
Write-Output "Setting Global Mode..."
Invoke-WebRequest -Uri "http://127.0.0.1:8080/system/proxy" -Method Post -Body '{"mode":"global"}' -WebSession $session -ContentType "application/json" | Out-Null

Write-Output "Settings after Global:"
Get-ProxySettings

# Set Off
Write-Output "Setting Off..."
Invoke-WebRequest -Uri "http://127.0.0.1:8080/system/proxy" -Method Post -Body '{"mode":"off"}' -WebSession $session -ContentType "application/json" | Out-Null

Write-Output "Settings after Off:"
Get-ProxySettings
