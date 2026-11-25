# Measures consensus + execution disk usage inside the stack, appends to
# data/measurements.csv and exits 1 if the total exceeds the hard limit.
param([string]$Limit = "25GB", [string]$Target = "20GB")
Set-Location (Split-Path -Parent $PSScriptRoot)
. "$PSScriptRoot\require-docker.ps1"; Assert-Docker
docker compose exec -T execution litecheck disk -dirs /blockchain/consensus,/blockchain/execution `
    -limit $Limit -target $Target -csv /blockchain/measurements.csv
exit $LASTEXITCODE
