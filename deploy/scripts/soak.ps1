# Resumable multi-hour soak. Every -IntervalMin minutes: measure disk (hard
# limit => exit 1), record health/validation metrics, and run the functional
# RPC comparison every -CompareEvery samples.
#
# Progress is saved to data\soak-state.json after every sample. Re-running
# the script (e.g. after a reboot) continues where it stopped; only time with
# the stack up counts toward -Hours. Use -Fresh to discard saved progress.
# See soak-autoresume.ps1 to resume automatically at logon.
param(
    [double]$Hours = 4,
    [int]$IntervalMin = 10,
    [int]$CompareEvery = 3,
    [string]$Limit = "25GB",
    [string]$DataDir = $(if ($env:DATA_DIR) { $env:DATA_DIR } else { "pulselite-data" }),
    [switch]$Fresh
)
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
$env:DATA_DIR = $DataDir
$mutex = New-Object System.Threading.Mutex($false, "Local\PulseLiteSoak")
if (-not $mutex.WaitOne(0)) { Write-Host "Another soak is already running; exiting."; exit 0 }
New-Item -ItemType Directory -Force -Path (Join-Path $Root "data") | Out-Null
$log = Join-Path $Root "data\soak.log"
$stateFile = Join-Path $Root "data\soak-state.json"

function Write-Log([string]$msg) { $msg | Tee-Object -FilePath $log -Append }

function Save-State($s) {
    $tmp = "$stateFile.tmp"
    $s | ConvertTo-Json | Set-Content -Path $tmp -Encoding UTF8
    Move-Item -Force $tmp $stateFile
}

function Wait-Docker {
    docker info --format '{{.ServerVersion}}' *> $null
    if ($LASTEXITCODE -eq 0) { return }
    $exe = @("$env:ProgramFiles\Docker\Docker\Docker Desktop.exe", "$env:LOCALAPPDATA\Programs\DockerDesktop\Docker Desktop.exe") |
        Where-Object { Test-Path $_ } | Select-Object -First 1
    if ($exe) { Write-Log "Starting Docker Desktop..."; Start-Process $exe }
    for ($i = 0; $i -lt 120; $i++) {
        Start-Sleep 5
        docker info --format '{{.ServerVersion}}' *> $null
        if ($LASTEXITCODE -eq 0) { return }
    }
    throw "Docker daemon did not come up within 10 minutes"
}

function Ensure-Stack {
    $running = docker compose ps --status running --services 2>$null
    if (-not ($running -contains "execution") -or -not (($running -contains "lighthouse") -or ($running -contains "prysm"))) {
        Write-Log "Stack not running, starting it (DATA_DIR=$DataDir)..."
        docker compose up -d *> $null
    }
    for ($i = 0; $i -lt 90; $i++) {
        docker compose exec -T execution litecheck health *> $null
        if ($LASTEXITCODE -eq 0) { return }
        Start-Sleep 5
    }
    Write-Log "WARN: execution health endpoint not answering yet; sampling anyway"
}

$state = $null
if ((Test-Path $stateFile) -and -not $Fresh) {
    $state = Get-Content $stateFile -Raw | ConvertFrom-Json
    if ($state.complete) {
        Write-Log "Previous soak already complete ($([math]::Round($state.accumulatedSec/3600,2)) h). Use -Fresh to start a new one."
        exit 0
    }
    Write-Log ("=== resuming soak: {0:N2} of {1} h done, {2} samples, {3} interruptions so far" -f ($state.accumulatedSec / 3600), $state.targetHours, $state.samples, $state.interruptions)
} else {
    $state = [pscustomobject]@{
        targetHours = $Hours; intervalMin = $IntervalMin; limit = $Limit; dataDir = $DataDir
        startedUtc = (Get-Date).ToUniversalTime().ToString("o"); lastSampleUtc = $null
        accumulatedSec = 0.0; samples = 0; interruptions = 0; downtimeSec = 0.0; complete = $false
    }
    Write-Log "=== new soak: $Hours h, sample every $IntervalMin min, hard limit $Limit, DATA_DIR=$DataDir"
    Save-State $state
}
$target = [double]$state.targetHours * 3600
$maxGap = [double]$state.intervalMin * 60 * 2 + 120

while ($true) {
    Wait-Docker
    Ensure-Stack
    $now = (Get-Date).ToUniversalTime()
    if ($state.lastSampleUtc) {
        $gap = ($now - [datetime]::Parse($state.lastSampleUtc).ToUniversalTime()).TotalSeconds
        if ($gap -le $maxGap) {
            $state.accumulatedSec += $gap
        } else {
            $state.interruptions += 1
            $state.downtimeSec += $gap
            Write-Log ("=== interruption #{0}: {1:N1} min without samples (not counted)" -f $state.interruptions, ($gap / 60))
        }
    }
    $state.samples += 1
    $state.lastSampleUtc = $now.ToString("o")
    Write-Log ("=== sample {0} at {1:u}  ({2:N2} / {3} h operating time)" -f $state.samples, $now, ($state.accumulatedSec / 3600), $state.targetHours)

    $disk = docker compose exec -T execution litecheck disk -dirs /blockchain/consensus,/blockchain/execution -limit $state.limit -csv /blockchain/measurements.csv 2>&1
    $disk | Tee-Object -FilePath $log -Append
    if (($disk | Out-String) -match "exceeds hard limit") { Save-State $state; Write-Log "FAIL: disk hard limit exceeded"; exit 1 }
    docker compose exec -T execution litecheck health 2>&1 | Tee-Object -FilePath $log -Append
    try {
        ((Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 "http://127.0.0.1:6060/metrics").Content -split "`n") -match '^pulselite_(newpayload_total|exec_seconds|cache_rotations|proof_verifications|sibling)' | Tee-Object -FilePath $log -Append
        (Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 "http://127.0.0.1:3500/eth/v1/node/syncing").Content | Tee-Object -FilePath $log -Append
    } catch { Write-Log "metrics/syncing unavailable: $($_.Exception.Message)" }
    if ($state.samples % $CompareEvery -eq 1) {
        docker compose exec -T execution litecheck compare -accounts 15 2>&1 | Select-Object -Last 16 | Tee-Object -FilePath $log -Append
    }
    Save-State $state

    if ($state.accumulatedSec -ge $target) { break }
    Start-Sleep -Seconds ([int]$state.intervalMin * 60)
}

$state.complete = $true
Save-State $state
Write-Log ("PASS: soak completed: {0:N2} h operating time, {1} samples, {2} interruptions ({3:N1} min downtime), within {4}" -f `
    ($state.accumulatedSec / 3600), $state.samples, $state.interruptions, ($state.downtimeSec / 60), $state.limit)
if (Get-ScheduledTask -TaskName "PulseLiteSoakResume" -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName "PulseLiteSoakResume" -Confirm:$false
    Write-Log "Removed auto-resume scheduled task."
}
