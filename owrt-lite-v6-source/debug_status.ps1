
$ErrorActionPreference = "Stop"
$AdminUrl = "http://127.0.0.1:8080"

# 1. Login
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
try {
    Invoke-WebRequest -Uri "$AdminUrl/login" -Method Post -Body '{"password":"admin"}' -WebSession $session -ContentType "application/json" | Out-Null
} catch {
    Write-Host "Login failed or service not running."
    exit
}

# 2. Get Node List and Status
try {
    $resp = Invoke-WebRequest -Uri "$AdminUrl/node/list" -Method Get -WebSession $session
    $data = $resp.Content | ConvertFrom-Json
    
    Write-Host "--- Debug Info ---"
    Write-Host "Active Node ID: '$($data.active)'"
    Write-Host "Proxy Mode: '$($data.proxy_mode)'"
    Write-Host "Node Count: $($data.nodes.Count)"
    
    if ($data.nodes.Count -gt 0) {
        Write-Host "Nodes:"
        foreach ($n in $data.nodes) {
            Write-Host " - [$($n.id)] $($n.name) (Healthy: $($n.healthy))"
        }
    } else {
        Write-Host "No nodes configured!"
    }
} catch {
    Write-Host "Failed to get node list: $($_.Exception.Message)"
}
