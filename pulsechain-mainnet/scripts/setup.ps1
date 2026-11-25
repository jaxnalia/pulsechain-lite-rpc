$ErrorActionPreference = "Stop"

$RootDir = Split-Path -Parent $PSScriptRoot
$DataDir = Join-Path $RootDir "data"
$JwtFile = Join-Path $DataDir "jwt.hex"

New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

if (-not (Test-Path $JwtFile) -or (Get-Item $JwtFile).Length -eq 0) {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $hex = ($bytes | ForEach-Object { $_.ToString("x2") }) -join ""
    [System.IO.File]::WriteAllText($JwtFile, $hex)
    Write-Host "Created $JwtFile"
} else {
    Write-Host "JWT already exists at $JwtFile"
}

Write-Host "Setup complete. Next: docker compose up -d"
