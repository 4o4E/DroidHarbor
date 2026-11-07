[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$NewBinary,
    [Parameter(Mandatory=$true)][string]$InstalledBinary
)
$ErrorActionPreference = 'Stop'
if ($NewBinary -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $NewBinary -PathType Leaf)) {
    throw 'NewBinary must be an existing absolute file path.'
}
if ($InstalledBinary -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $InstalledBinary -PathType Leaf)) {
    throw 'InstalledBinary must be an existing absolute file path.'
}
$services = @('DroidHarbor', 'DroidHarborADB')
$wasRunning = @{}
foreach ($name in $services) {
    $service = Get-Service -Name $name -ErrorAction Stop
    $wasRunning[$name] = $service.Status -eq [System.ServiceProcess.ServiceControllerStatus]::Running
}
$stamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$backup = "$InstalledBinary.backup-$stamp"
$stage = "$InstalledBinary.update-$PID"
if ((Test-Path -LiteralPath $backup) -or (Test-Path -LiteralPath $stage)) {
    throw 'Update backup or staging file already exists; refusing overwrite.'
}
$failed = "$InstalledBinary.failed-$stamp"
if (Test-Path -LiteralPath $failed) { throw 'Failed-binary retention path already exists; refusing overwrite.' }
try {
    foreach ($name in $services) {
        if ($wasRunning[$name]) {
            Stop-Service -Name $name
            (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
        }
    }
    Copy-Item -LiteralPath $NewBinary -Destination $stage
    & icacls.exe $stage /grant 'NT SERVICE\DroidHarbor:RX' 'NT SERVICE\DroidHarborADB:RX' /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Could not grant both service identities access to the staged binary.' }
    [System.IO.File]::Move($InstalledBinary, $backup)
    try {
        [System.IO.File]::Move($stage, $InstalledBinary)
    }
    catch {
        [System.IO.File]::Move($backup, $InstalledBinary)
        throw
    }
    foreach ($name in @('DroidHarborADB', 'DroidHarbor')) {
        if ($wasRunning[$name]) {
            Start-Service -Name $name
            (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(45))
        }
    }
    Write-Output "Services updated. Previous binary retained at '$backup'."
}
catch {
    foreach ($name in @('DroidHarbor', 'DroidHarborADB')) {
        if ($wasRunning[$name]) {
            $service = Get-Service -Name $name
            if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
                Stop-Service -Name $name -Force
                $service.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
            }
        }
    }
    if (Test-Path -LiteralPath $backup) {
        if (Test-Path -LiteralPath $InstalledBinary) {
            [System.IO.File]::Move($InstalledBinary, $failed)
        }
        [System.IO.File]::Move($backup, $InstalledBinary)
    }
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Force }
    foreach ($name in @('DroidHarborADB', 'DroidHarbor')) {
        if ($wasRunning[$name]) {
            Start-Service -Name $name
            (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(45))
        }
    }
    throw
}
