# Dot-source from a deploy script, then call Assert-Docker:
#   . "$PSScriptRoot\require-docker.ps1"; Assert-Docker
# Makes sure the Docker engine is reachable (starting Docker Desktop if it is
# installed but not running) and exits with a plain message otherwise.

function Stop-WithMessage([string]$Message) {
    Write-Host ""
    Write-Host $Message -ForegroundColor Red
    exit 1
}

function Test-DockerEngine {
    $ErrorActionPreference = "SilentlyContinue" # docker writes to stderr when the engine is down
    $null = & docker info 2>&1
    return $LASTEXITCODE -eq 0
}

function Assert-Docker([int]$WaitSeconds = 180) {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Stop-WithMessage "Docker is not installed. Install Docker Desktop (https://www.docker.com/products/docker-desktop/), start it, and try again."
    }
    if (-not (Test-DockerEngine)) {
        $desktop = @(
            $(if ($env:ProgramFiles) { Join-Path $env:ProgramFiles "Docker\Docker\Docker Desktop.exe" }),
            $(if ($env:LOCALAPPDATA) { Join-Path $env:LOCALAPPDATA "Programs\DockerDesktop\Docker Desktop.exe" })
        ) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
        if (-not $desktop) {
            Stop-WithMessage "Docker is not running. Please start Docker and try again."
        }
        Write-Host "Docker is not running. Starting Docker Desktop (this can take a minute)..."
        Start-Process $desktop
        $deadline = (Get-Date).AddSeconds($WaitSeconds)
        while (-not (Test-DockerEngine)) {
            if ((Get-Date) -gt $deadline) {
                Stop-WithMessage "Unable to start Docker. Please start Docker Desktop, wait until it shows 'Engine running', and try again."
            }
            Start-Sleep -Seconds 3
        }
        Write-Host "Docker is running."
    }
    $ErrorActionPreference = "SilentlyContinue"
    $null = & docker compose version 2>&1
    if ($LASTEXITCODE -ne 0) {
        Stop-WithMessage "Docker Compose v2 ('docker compose') is required. Update Docker Desktop and try again."
    }
}
