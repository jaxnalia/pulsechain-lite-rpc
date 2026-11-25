# Compares pulselite answers against an independent reference node at the
# same block (balances, nonces, code, storage, eth_call, estimateGas, logs,
# receipts). Exits 1 on any mismatch.
param([string]$Ref = "https://rpc.pulsechain.com", [int]$Accounts = 25)
Set-Location (Split-Path -Parent $PSScriptRoot)
. "$PSScriptRoot\require-docker.ps1"; Assert-Docker
docker compose exec -T execution litecheck compare -local http://127.0.0.1:8545 -ref $Ref -accounts $Accounts
exit $LASTEXITCODE
