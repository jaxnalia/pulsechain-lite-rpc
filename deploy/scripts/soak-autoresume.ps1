# Registers (or removes) a per-user scheduled task that resumes soak.ps1 at
# logon, so a soak interrupted by a reboot/shutdown continues automatically.
# soak.ps1 removes the task itself once the soak completes.
#   .\scripts\soak-autoresume.ps1            # install
#   .\scripts\soak-autoresume.ps1 -Remove    # uninstall
param(
    [switch]$Remove,
    [string]$DataDir = $(if ($env:DATA_DIR) { $env:DATA_DIR } else { "pulselite-data" })
)
$name = "PulseLiteSoakResume"
if ($Remove) {
    Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue
    Write-Host "Removed scheduled task $name"
    exit 0
}
$script = Join-Path $PSScriptRoot "soak.ps1"
$action = New-ScheduledTaskAction -Execute "powershell.exe" `
    -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Minimized -File `"$script`" -DataDir $DataDir"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$trigger.Delay = "PT2M"  # give Docker Desktop a head start
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName $name -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Write-Host "Installed scheduled task ${name}: soak resumes 2 minutes after you log on (DATA_DIR=$DataDir)."
