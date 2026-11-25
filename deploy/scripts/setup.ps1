# One-command startup of the PulseChain Lite RPC:
#   .\scripts\setup.ps1 [-Lan] [-Proxy] [-Consensus lighthouse|prysm] [-NoWait]
# Experimental validator modes (blocks not yet validated within the slot):
#   .\scripts\setup.ps1 -Mode validator|full -ExperimentalValidator [-Validator]
param(
    [ValidateSet("rpc", "validator", "full")] [string]$Mode = "rpc",
    [ValidateSet("lighthouse", "prysm")] [string]$Consensus = "lighthouse",
    [switch]$Lan,
    [switch]$Proxy,
    [switch]$Validator,
    [switch]$ExperimentalValidator,
    [switch]$NoWait,
    [int]$WaitMinutes = 20
)
$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

. "$PSScriptRoot\require-docker.ps1"

$experimental = $Mode -ne "rpc" -or $Validator
if ($experimental -and -not $ExperimentalValidator) {
    Stop-WithMessage "Validator modes are disabled in this release (blocks are not validated fast enough over p2p yet). Use the default rpc mode, or add -ExperimentalValidator to run them anyway."
}
if ($Validator -and $Mode -eq "rpc") {
    Stop-WithMessage "-Validator needs -Mode validator or -Mode full."
}
Assert-Docker

$profiles = @($Consensus)
if ($Proxy) { $profiles += "proxy" }
if ($Validator) { $profiles += $(if ($Consensus -eq "prysm") { "validator-prysm" } else { "validator" }) }

$EnvFile = Join-Path $Root ".env"
$envText = Get-Content $EnvFile -Raw
function Set-EnvValue([string]$key, [string]$value) {
    if ($script:envText -match "(?m)^$key=") {
        $script:envText = [regex]::Replace($script:envText, "(?m)^$key=.*$", "$key=$value")
    } else {
        $script:envText = $script:envText.TrimEnd() + "`n$key=$value`n"
    }
}
Set-EnvValue "PULSELITE_MODE" $Mode
Set-EnvValue "PULSELITE_EXPERIMENTAL_VALIDATOR" $(if ($experimental) { "true" } else { "false" })
Set-EnvValue "LIGHTHOUSE_EXTRA_FLAGS" $(if ($experimental) { "" } else { "--disable-deposit-contract-sync" })
Set-EnvValue "LH_FRESH_START" $(if ($experimental) { "false" } else { "true" })
Set-EnvValue "LH_DB_BUDGET_MB" $(if ($experimental) { "0" } else { "2000" })
Set-EnvValue "COMPOSE_PROFILES" ($profiles -join ",")
Set-EnvValue "RPC_BIND" $(if ($Lan) { "0.0.0.0" } else { "127.0.0.1" })
[IO.File]::WriteAllText($EnvFile, $envText)

# A host-path DATA_DIR gets its JWT here; a named volume gets it from pulselite.
$dataDir = ([regex]::Match($envText, "(?m)^DATA_DIR=(.*)$").Groups[1].Value).Trim()
if ($dataDir -match '^(\.|/|[A-Za-z]:)') {
    $DataPath = if ([IO.Path]::IsPathRooted($dataDir)) { $dataDir } else { Join-Path $Root $dataDir }
    New-Item -ItemType Directory -Force -Path $DataPath | Out-Null
    $Jwt = Join-Path $DataPath "jwt.hex"
    if (-not (Test-Path $Jwt) -or (Get-Item $Jwt).Length -eq 0) {
        $bytes = New-Object byte[] 32
        [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
        [IO.File]::WriteAllText($Jwt, (($bytes | ForEach-Object { $_.ToString("x2") }) -join ""))
        Write-Host "Created shared JWT secret $Jwt"
    }
}

Write-Host "Starting PulseChain Lite: mode=$Mode consensus=$Consensus profiles=$($profiles -join ',')"
docker compose up -d --build
if ($LASTEXITCODE -ne 0) { Stop-WithMessage "Starting the containers failed (see the messages above). Fix the problem and run setup again." }

$port = ([regex]::Match($envText, "(?m)^RPC_PORT=(\d+)").Groups[1].Value)
if (-not $port) { $port = "8545" }
$url = "http://127.0.0.1:$port"
Write-Host ""
Write-Host "Dashboard + RPC: $url  (open it in a browser)"
if ($Lan) { Write-Host "From other devices: http://<this-computer's-LAN-IP>:$port" }
if ($NoWait) { exit 0 }

Write-Host "Waiting for the node to become ready (Ctrl+C to stop waiting; the node keeps running)..."
$deadline = (Get-Date).AddMinutes($WaitMinutes)
$last = ""
while ((Get-Date) -lt $deadline) {
    try {
        $s = Invoke-RestMethod -TimeoutSec 5 "$url/api/status"
    } catch {
        $s = $null
    }
    if ($s -and $s.ready) {
        Write-Host ""
        Write-Host "READY: serving verified state and accepting transactions." -ForegroundColor Green
        Write-Host "Wallet settings: RPC URL $url | Chain ID 369 | Symbol PLS | Explorer https://scan.pulsechain.com"
        exit 0
    }
    $msg = if ($s) { $s.summary } else { "starting containers..." }
    if ($msg -ne $last) { Write-Host ("  [{0}] {1}" -f (Get-Date -Format HH:mm:ss), $msg); $last = $msg }
    Start-Sleep -Seconds 5
}
Write-Host "Not ready after $WaitMinutes minutes; it may still be syncing. Check $url or 'docker compose logs -f'." -ForegroundColor Yellow
