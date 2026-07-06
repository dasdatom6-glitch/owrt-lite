
$Proxy = "http://127.0.0.1:8080"
$Target = "http://example.com"

Write-Host "Testing connection to $Target via $Proxy"

try {
    $response = Invoke-WebRequest -Uri $Target -Proxy $Proxy -TimeoutSec 5
    Write-Host "Success! Status: $($response.StatusCode)"
    Write-Host "Content Length: $($response.Content.Length)"
} catch {
    Write-Host "Error: $($_.Exception.Message)"
    if ($_.Exception.Response) {
        Write-Host "Response Status: $($_.Exception.Response.StatusCode)"
        $reader = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
        Write-Host "Response Body: $($reader.ReadToEnd())"
    }
}
