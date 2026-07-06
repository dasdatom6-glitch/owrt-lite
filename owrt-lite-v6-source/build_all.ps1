$ErrorActionPreference = "Continue"
$logFile = "build.log"
"Starting Build..." | Out-File $logFile -Encoding utf8

$binDir = "bin"
if (-not (Test-Path -Path $binDir)) {
    New-Item -ItemType Directory -Path $binDir | Out-Null
}

$targets = @(
    @{OS="linux"; Arch="amd64"; Name="owrt-lite-linux-amd64"},
    @{OS="linux"; Arch="386"; Name="owrt-lite-linux-386"},
    @{OS="linux"; Arch="arm"; Name="owrt-lite-linux-armv7"; Env=@{GOARM="7"}},
    @{OS="linux"; Arch="arm64"; Name="owrt-lite-linux-arm64"},
    @{OS="linux"; Arch="mipsle"; Name="owrt-lite-linux-mipsle-soft"; Env=@{GOMIPS="softfloat"}},
    @{OS="linux"; Arch="mips"; Name="owrt-lite-linux-mips-soft"; Env=@{GOMIPS="softfloat"}},
    
    @{OS="windows"; Arch="amd64"; Name="owrt-lite-windows-amd64.exe"},
    @{OS="windows"; Arch="386"; Name="owrt-lite-windows-386.exe"},
    @{OS="windows"; Arch="arm64"; Name="owrt-lite-windows-arm64.exe"},
    
    @{OS="darwin"; Arch="amd64"; Name="owrt-lite-darwin-amd64"},
    @{OS="darwin"; Arch="arm64"; Name="owrt-lite-darwin-arm64"},
    
    @{OS="freebsd"; Arch="amd64"; Name="owrt-lite-freebsd-amd64"}
)

$env:CGO_ENABLED = "0"

foreach ($t in $targets) {
    $env:GOOS = $t.OS
    $env:GOARCH = $t.Arch
    
    # Reset specific env vars to avoid pollution
    if ($env:GOARM) { Remove-Item Env:\GOARM }
    if ($env:GOMIPS) { Remove-Item Env:\GOMIPS }

    if ($t.Env) {
        foreach ($key in $t.Env.Keys) {
            Set-Item -Path "Env:$key" -Value $t.Env[$key]
        }
    }

    $outPath = Join-Path $binDir $t.Name
    "Building for $($t.OS)/$($t.Arch)..." | Out-File $logFile -Append
    
    go build -trimpath -ldflags "-s -w" -o $outPath ./cmd/owrt-lite 2>>$logFile
    
    if ($LASTEXITCODE -eq 0) {
        "  -> Success: $outPath" | Out-File $logFile -Append
    } else {
        "  -> Failed" | Out-File $logFile -Append
    }
}

# Clean up env
Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\GOARM -ErrorAction SilentlyContinue
Remove-Item Env:\GOMIPS -ErrorAction SilentlyContinue
Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue

"All builds completed." | Out-File $logFile -Append
