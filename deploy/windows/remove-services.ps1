[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
foreach ($name in @('DroidHarbor', 'DroidHarborADB')) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($null -eq $service) { continue }
    if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        Stop-Service -Name $name
        (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
    }
    & sc.exe delete $name | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not remove service registration '$name'." }
}
Write-Output 'DroidHarbor service registrations removed. Binaries, database, TLS files, and ADB keys were left untouched.'
