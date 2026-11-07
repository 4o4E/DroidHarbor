[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BackupDestination,
    [Parameter(Mandatory=$true)][string]$DroidHarborBinary,
    [Parameter(Mandatory=$true)][string]$PlatformDataDirectory,
    [Parameter(Mandatory=$true)][string]$ConfigDirectory
)
$ErrorActionPreference = 'Stop'
function Require-Directory([string]$Path, [string]$Name) {
    if ($Path -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "$Name must be an existing absolute directory."
    }
}
function Copy-Tree([string]$Source, [string]$Destination) {
    & robocopy.exe $Source $Destination /E /COPYALL /DCOPY:DAT /XJ /R:1 /W:1 /NFL /NDL /NJH /NJS /NP | Out-Null
    if ($LASTEXITCODE -ge 8) { throw 'A backup component could not be copied.' }
}
function Get-AdbServiceAndroidDirectory {
    $serviceAccount = New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList 'NT SERVICE\DroidHarborADB'
    $sid = $serviceAccount.Translate([System.Security.Principal.SecurityIdentifier]).Value
    $profiles = @(Get-CimInstance -ClassName Win32_UserProfile -Filter "SID='$sid'")
    if ($profiles.Count -ne 1 -or [string]::IsNullOrWhiteSpace($profiles[0].LocalPath)) {
        throw 'Windows has no unique registered profile for the DroidHarborADB service account; refusing to guess its ADB key location.'
    }
    $profilePath = [System.IO.Path]::GetFullPath($profiles[0].LocalPath)
    $androidPath = Join-Path $profilePath '.android'
    Require-Directory $androidPath 'DroidHarborADB actual .android key directory'
    if (-not (Test-Path -LiteralPath (Join-Path $androidPath 'adbkey') -PathType Leaf)) {
        throw 'The official ADB default private key is missing from the resolved service-account profile; refusing an incomplete backup.'
    }
    return $androidPath
}

Require-Directory $PlatformDataDirectory 'PlatformDataDirectory'
Require-Directory $ConfigDirectory 'ConfigDirectory'
$adbAndroidDirectory = Get-AdbServiceAndroidDirectory
if ($DroidHarborBinary -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $DroidHarborBinary -PathType Leaf)) {
    throw 'DroidHarborBinary must be an existing absolute file.'
}
if ($BackupDestination -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or (Test-Path -LiteralPath $BackupDestination)) {
    throw 'BackupDestination must be a new absolute path; overwrite is forbidden.'
}
$parent = Split-Path -Parent $BackupDestination
if (-not (Test-Path -LiteralPath $parent -PathType Container)) { throw 'BackupDestination parent directory must exist.' }
$backupFull = [System.IO.Path]::GetFullPath($BackupDestination).TrimEnd('\') + '\'
foreach ($source in @($PlatformDataDirectory, $ConfigDirectory, $adbAndroidDirectory)) {
    $sourceFull = [System.IO.Path]::GetFullPath($source).TrimEnd('\') + '\'
    if ($sourceFull -eq [System.IO.Path]::GetPathRoot($sourceFull) -or
        $backupFull.StartsWith($sourceFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'BackupDestination must be outside a non-root backup source directory.'
    }
}
$wasRunning = @{}
foreach ($name in @('DroidHarbor', 'DroidHarborADB')) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    $wasRunning[$name] = $null -ne $service -and $service.Status -eq [System.ServiceProcess.ServiceControllerStatus]::Running
}
New-Item -ItemType Directory -Path $BackupDestination | Out-Null
try {
    foreach ($name in @('DroidHarbor', 'DroidHarborADB')) {
        if ($wasRunning[$name]) {
            Stop-Service -Name $name
            (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
        }
    }
    if ((Get-Service -Name 'DroidHarbor' -ErrorAction SilentlyContinue | Where-Object Status -ne Stopped) -or
        (Get-Service -Name 'DroidHarborADB' -ErrorAction SilentlyContinue | Where-Object Status -ne Stopped)) {
        throw 'Services did not stop; backup aborted.'
    }
    New-Item -ItemType Directory -Path (Join-Path $BackupDestination 'platform-data'), (Join-Path $BackupDestination 'config'), (Join-Path $BackupDestination 'adb-auth') | Out-Null
    Copy-Tree $PlatformDataDirectory (Join-Path $BackupDestination 'platform-data')
    Copy-Tree $ConfigDirectory (Join-Path $BackupDestination 'config')
    Copy-Tree $adbAndroidDirectory (Join-Path (Join-Path $BackupDestination 'adb-auth') '.android')
    $versionLines = & $DroidHarborBinary version
    if ($LASTEXITCODE -ne 0) { throw 'Could not read the backup-producing binary version.' }
    $versionText = $versionLines -join [Environment]::NewLine
    $versionMetadata = $versionText | ConvertFrom-Json -ErrorAction Stop
    if ($versionMetadata.product -ne 'droidharbor' -or [int]$versionMetadata.apiMajor -lt 1 -or [int]$versionMetadata.schemaVersion -lt 1) {
        throw 'Backup-producing binary returned invalid version metadata.'
    }
    [System.IO.File]::WriteAllText((Join-Path $BackupDestination 'platform-version.json'), $versionText)
    [System.IO.File]::WriteAllText((Join-Path $BackupDestination 'manifest.txt'), "format=droidharbor-backup-v1`r`n")
    Write-Output "Stopped-state backup created at '$BackupDestination'. Platform state/config and ADB authorization are separate."
}
finally {
    foreach ($name in @('DroidHarborADB', 'DroidHarbor')) {
        if ($wasRunning[$name]) {
            $service = Get-Service -Name $name -ErrorAction SilentlyContinue
            if ($null -ne $service) {
                if ($service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
                    $service.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
                }
                Start-Service -Name $name
                (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(45))
            }
        }
    }
}
