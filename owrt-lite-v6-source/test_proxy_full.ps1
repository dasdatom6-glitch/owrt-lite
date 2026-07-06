
$ErrorActionPreference = "Stop"
$ProxyUrl = "http://127.0.0.1:8080"
$AdminUrl = "http://127.0.0.1:8080"

function Invoke-Admin {
    param($Path, $Method="GET", $Body=$null)
    $params = @{
        Uri = "$AdminUrl$Path"
        Method = $Method
        WebSession = $session
        ContentType = "application/json"
    }
    if ($Body) { $params.Body = $Body }
    Invoke-WebRequest @params
}

# 1. Login
Write-Host "Logging in..."
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
Invoke-WebRequest -Uri "$AdminUrl/login" -Method Post -Body '{"password":"admin"}' -WebSession $session -ContentType "application/json" | Out-Null

# 2. List Nodes
Write-Host "Listing nodes..."
$nodesResp = Invoke-Admin -Path "/node/list"
$nodesData = $nodesResp.Content | ConvertFrom-Json
$nodes = $nodesData.nodes

$selectedId = ""

if ($nodes.Count -gt 0) {
    $node = $nodes[0]
    $selectedId = $node.id
    Write-Host "Found existing node: $($node.name) ($selectedId)"
} else {
    Write-Host "No nodes found. Adding dummy node..."
    # Add dummy node
    $dummyBody = '{"name":"Dummy","protocol":"http","host":"example.com","port":"80"}'
    Invoke-Admin -Path "/node/add/manual" -Method Post -Body $dummyBody | Out-Null
    
    # List again to get ID
    $nodesResp = Invoke-Admin -Path "/node/list"
    $nodesData = $nodesResp.Content | ConvertFrom-Json
    $selectedId = $nodesData.nodes[0].id
    Write-Host "Added dummy node: $selectedId"
}

# 3. Select Node
Write-Host "Selecting node: $selectedId"
Invoke-Admin -Path "/node/active" -Method Post -Body "{`"id`":`"$selectedId`"}" | Out-Null

# 4. Set Global Mode
Write-Host "Setting Global Mode..."
Invoke-Admin -Path "/system/proxy" -Method Post -Body '{"mode":"global"}' | Out-Null

# 5. Test Proxy
Write-Host "Testing Proxy Connection..."
try {
    $response = Invoke-WebRequest -Uri "http://google.com" -Proxy $ProxyUrl -TimeoutSec 5
    Write-Host "Proxy Success! Status: $($response.StatusCode)"
} catch {
    Write-Host "Proxy Request Failed: $($_.Exception.Message)"
    if ($_.Exception.Response) {
        Write-Host "Response Status: $($_.Exception.Response.StatusCode)"
        $reader = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
        Write-Host "Response Body: $($reader.ReadToEnd())"
    }
}
