


$ErrorActionPreference = "Stop"
$AdminUrl = "http://127.0.0.1:8080"

# 1. Start Server in background
$proc = Start-Process -FilePath ".\bin\owrt-lite.exe" -PassThru -NoNewWindow
Start-Sleep -Seconds 3

try {
    # 2. Login
    $session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
    Invoke-WebRequest -Uri "$AdminUrl/login" -Method Post -Body '{"password":"admin"}' -WebSession $session -ContentType "application/json" | Out-Null

    # 3. Add a test node (SOCKS5 if possible, or just use an existing one)
    # Let's assume there's already a node or we add a dummy one.
    # For now, let's just set mode to "global" and see if it falls back to direct.
    Invoke-WebRequest -Uri "$AdminUrl/system/proxy" -Method Post -Body '{"mode":"global"}' -WebSession $session -ContentType "application/json" | Out-Null

    # 4. Test request through proxy
    Write-Host "Testing request through 127.0.0.1:8080..."
    $proxy = New-Object System.Net.WebProxy("http://127.0.0.1:8080")
    $client = New-Object System.Net.WebClient
    $client.Proxy = $proxy
    
    try {
        Write-Host "Testing HTTPS request through proxy..."
        $html = $client.DownloadString("https://www.baidu.com")
        Write-Host "Successfully reached HTTPS baidu.com through proxy!"
        Write-Host "HTML length: $($html.Length)"
    } catch {
        Write-Host "Failed to reach HTTPS baidu.com through proxy: $($_.Exception.Message)"
    }

} finally {
    Stop-Process -Id $proc.Id -Force
}
