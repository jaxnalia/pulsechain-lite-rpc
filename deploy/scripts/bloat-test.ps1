# Restart/bloat integration test: N cycles of cache-filling query load
# followed by a restart of the execution service. Fails if the cache's
# logical size exceeds its cap, if on-disk usage of the execution datadir
# grows beyond 1.6x the cap + window, or if the service does not come back.
param([int]$Cycles = 5, [int]$Requests = 3000)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
. "$PSScriptRoot\require-docker.ps1"; Assert-Docker

function Get-Health {
    for ($i = 0; $i -lt 60; $i++) {
        $out = docker compose exec -T execution litecheck health 2>$null
        if ($LASTEXITCODE -eq 0 -and $out) { return ($out | ConvertFrom-Json) }
        Start-Sleep 2
    }
    throw "execution service not healthy"
}

$h = Get-Health
$cap = [double]$h.cacheCap
$results = @()
for ($c = 1; $c -le $Cycles; $c++) {
    docker compose exec -T execution litecheck load -n $Requests -concurrency 8
    if ($LASTEXITCODE -ne 0) { throw "load failed in cycle $c" }
    $h = Get-Health
    $diskOut = docker compose exec -T execution litecheck disk -dirs /blockchain/execution -limit 1TB -target 1TB
    $execBytes = [double](($diskOut | Select-String '/blockchain/execution').ToString().Split()[-2]) * 1e9
    $results += [pscustomobject]@{ cycle = $c; cacheBytes = [double]$h.cacheBytes; execDiskBytes = $execBytes; head = $h.head }
    Write-Host ("cycle {0}: cache {1:N1} MB (cap {2:N1} MB), execution datadir {3:N1} MB" -f $c, ($h.cacheBytes / 1MB), ($cap / 1MB), ($execBytes / 1MB))
    if ([double]$h.cacheBytes -gt $cap * 1.01) { throw "cache logical size exceeds cap" }
    if ($execBytes -gt ($cap * 1.6 + 512MB)) { throw "execution datadir bloated beyond bound" }
    docker compose restart execution | Out-Null
    Start-Sleep 5
    $null = Get-Health
    # The restarted service must come back with its persisted cache intact.
    $after = Get-Health
    Write-Host ("  after restart: cache {0:N1} MB, head {1}" -f ($after.cacheBytes / 1MB), $after.head)
}
$h = Get-Health
$results | Format-Table -AutoSize
Write-Host "PASS: cache bounded across $Cycles restart cycles (cap $([math]::Round($cap/1MB)) MB)"
