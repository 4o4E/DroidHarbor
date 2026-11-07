[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BackupSource,
    [Parameter(Mandatory=$true)][string]$DroidHarborBinary,
    [Parameter(Mandatory=$true)][string]$PlatformDataTarget,
    [Parameter(Mandatory=$true)][string]$ConfigTarget
)
$ErrorActionPreference = 'Stop'
function Require-Absolute([string]$Path, [string]$Name) {
    if ($Path -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})') { throw "$Name must be absolute." }
}
function Copy-Tree([string]$Source, [string]$Destination) {
    & robocopy.exe $Source $Destination /E /COPYALL /DCOPY:DAT /XJ /R:1 /W:1 /NFL /NDL /NJH /NJS /NP | Out-Null
    if ($LASTEXITCODE -ge 8) { throw 'A restore component could not be copied.' }
}
function Get-AdbServiceAndroidDirectory {
    $serviceAccount = New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList 'NT SERVICE\DroidHarborADB'
    $sid = $serviceAccount.Translate([System.Security.Principal.SecurityIdentifier]).Value
    $profiles = @(Get-CimInstance -ClassName Win32_UserProfile -Filter "SID='$sid'")
    if ($profiles.Count -ne 1 -or [string]::IsNullOrWhiteSpace($profiles[0].LocalPath)) {
        throw 'Windows has no unique registered profile for the DroidHarborADB service account; refusing to guess its ADB key location.'
    }
    return Join-Path ([System.IO.Path]::GetFullPath($profiles[0].LocalPath)) '.android'
}
function Get-TreeManifest([string]$Path) {
    $root = [System.IO.Path]::GetFullPath($Path).TrimEnd('\') + '\'
    $files = @(Get-ChildItem -LiteralPath $Path -File -Force -Recurse | ForEach-Object {
        $relative = $_.FullName.Substring($root.Length)
        "$relative`0$((Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash)"
    } | Sort-Object)
    return $files -join "`n"
}
function Quote-WindowsArgument([string]$Value) {
    if ($Value.Contains('"')) { throw 'Service command arguments may not contain quote characters.' }
    $trailing = 0
    for ($index = $Value.Length - 1; $index -ge 0 -and $Value[$index] -eq [char]92; $index--) { $trailing++ }
    $prefix = $Value.Substring(0, $Value.Length - $trailing)
    return '"' + $prefix + ('\' * ($trailing * 2)) + '"'
}
Require-Absolute $BackupSource 'BackupSource'
Require-Absolute $PlatformDataTarget 'PlatformDataTarget'
Require-Absolute $ConfigTarget 'ConfigTarget'
Require-Absolute $DroidHarborBinary 'DroidHarborBinary'
if (-not (Test-Path -LiteralPath $BackupSource -PathType Container) -or
    -not (Test-Path -LiteralPath (Join-Path $BackupSource 'manifest.txt') -PathType Leaf) -or
    -not (Test-Path -LiteralPath (Join-Path $BackupSource 'platform-version.json') -PathType Leaf)) {
    throw 'Backup manifest or version record is missing.'
}
$manifest = (Get-Content -LiteralPath (Join-Path $BackupSource 'manifest.txt') -Raw).Trim()
if ($manifest -ne 'format=droidharbor-backup-v1') { throw 'Backup manifest format is unsupported.' }
foreach ($component in @('platform-data', 'config', 'adb-auth/.android')) {
    if (-not (Test-Path -LiteralPath (Join-Path $BackupSource $component) -PathType Container)) {
        throw "Backup component '$component' is missing."
    }
}
if (-not (Test-Path -LiteralPath (Join-Path $BackupSource 'adb-auth/.android/adbkey') -PathType Leaf)) {
    throw 'Backup is missing the official ADB default private key.'
}
if (-not (Test-Path -LiteralPath $DroidHarborBinary -PathType Leaf)) { throw 'DroidHarborBinary is unavailable.' }
$backupMetadata = Get-Content -LiteralPath (Join-Path $BackupSource 'platform-version.json') -Raw | ConvertFrom-Json -ErrorAction Stop
if ($backupMetadata.product -ne 'droidharbor') { throw 'Backup product metadata is invalid.' }
$currentVersionLines = & $DroidHarborBinary version
if ($LASTEXITCODE -ne 0) { throw 'Could not read the target binary version.' }
$currentMetadata = ($currentVersionLines -join [Environment]::NewLine) | ConvertFrom-Json -ErrorAction Stop
if ($currentMetadata.product -ne $backupMetadata.product -or
    [int]$currentMetadata.apiMajor -ne [int]$backupMetadata.apiMajor -or
    [int]$currentMetadata.schemaVersion -lt [int]$backupMetadata.schemaVersion) {
    throw 'Target binary is not API/schema compatible with this backup.'
}
foreach ($target in @($PlatformDataTarget, $ConfigTarget)) {
    if (Test-Path -LiteralPath $target) { throw "Restore target '$target' already exists; overwrite is forbidden." }
    $parent = Split-Path -Parent $target
    if (-not (Test-Path -LiteralPath $parent -PathType Container)) { throw "Restore target parent '$parent' must exist." }
}
$backupRoot = [System.IO.Path]::GetFullPath($BackupSource).TrimEnd('\') + '\'
foreach ($target in @($PlatformDataTarget, $ConfigTarget)) {
    if ([System.IO.Path]::GetFullPath($target).StartsWith($backupRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'Restore targets must not be inside the backup source.'
    }
}
foreach ($name in @('DroidHarbor', 'DroidHarborADB')) {
    $service = Get-Service -Name $name -ErrorAction SilentlyContinue
    if ($null -ne $service -and $service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        Stop-Service -Name $name
        (Get-Service -Name $name).WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
    }
}
if ((Get-Service -Name 'DroidHarbor' -ErrorAction SilentlyContinue | Where-Object Status -ne Stopped) -or
    (Get-Service -Name 'DroidHarborADB' -ErrorAction SilentlyContinue | Where-Object Status -ne Stopped)) {
    throw 'Services did not stop; restore aborted.'
}
$adbService = Get-CimInstance -ClassName Win32_Service -Filter "Name='DroidHarborADB'"
if ($null -eq $adbService -or [string]::IsNullOrWhiteSpace($adbService.PathName)) {
    throw 'DroidHarborADB is not installed; register it with install-services.ps1 -NoStart and the backup authorization seed first.'
}
$seedMatch = [regex]::Match($adbService.PathName, '(?i)"--auth-seed-dir"\s+"([^"]+)"')
if (-not $seedMatch.Success) {
    throw 'DroidHarborADB has no staged authorization seed. Install with -NoStart -AdbAuthorizationSeedDirectory pointing to this backup before restoring; ADB was not started.'
}
$seedStage = [System.IO.Path]::GetFullPath($seedMatch.Groups[1].Value)
$seedRoot = [System.IO.Path]::GetFullPath((Join-Path $env:ProgramData 'DroidHarbor\staging')).TrimEnd('\') + '\'
if (-not $seedStage.StartsWith($seedRoot, [System.StringComparison]::OrdinalIgnoreCase) -or
    [System.IO.Path]::GetFileName($seedStage) -notmatch '^adb-auth-[0-9a-f]{32}$' -or
    -not (Test-Path -LiteralPath $seedStage -PathType Container)) {
    throw 'SCM authorization seed is not a protected installer staging directory; refusing to trust or delete it.'
}
if ((Get-Item -LiteralPath $seedStage -Force).Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
    throw 'SCM authorization staging directory may not be a reparse point.'
}
$backupAuthDirectory = Join-Path $BackupSource 'adb-auth/.android'
if ((Get-TreeManifest $seedStage) -ne (Get-TreeManifest $backupAuthDirectory)) {
    throw 'Protected installer seed does not exactly match the selected backup; refusing to start ADB.'
}
try {
    Start-Service -Name 'DroidHarborADB'
    (Get-Service -Name 'DroidHarborADB').WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Running, [TimeSpan]::FromSeconds(45))
    Stop-Service -Name 'DroidHarborADB'
    (Get-Service -Name 'DroidHarborADB').WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
} catch {
    $runningAdb = Get-Service -Name 'DroidHarborADB' -ErrorAction SilentlyContinue
    if ($null -ne $runningAdb -and $runningAdb.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
        Stop-Service -Name 'DroidHarborADB' -Force -ErrorAction SilentlyContinue
        $runningAdb = Get-Service -Name 'DroidHarborADB' -ErrorAction SilentlyContinue
        if ($null -ne $runningAdb) {
            $runningAdb.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
        }
    }
    throw "ADB seed import/readiness failed; protected seed and SCM ImagePath are retained at '$seedStage'. $($_.Exception.Message)"
}
$adbAndroidDirectory = Get-AdbServiceAndroidDirectory
if (-not (Test-Path -LiteralPath $adbAndroidDirectory -PathType Container) -or
    (Get-TreeManifest $adbAndroidDirectory) -ne (Get-TreeManifest $backupAuthDirectory)) {
    throw "ADB reached readiness but its actual service-profile keys do not exactly match the backup; seed and SCM ImagePath remain at '$seedStage'."
}
$seedArgument = (Quote-WindowsArgument '--auth-seed-dir') + ' ' + (Quote-WindowsArgument $seedStage)
$registeredPath = $adbService.PathName
$seedOffset = $registeredPath.IndexOf($seedArgument, [System.StringComparison]::OrdinalIgnoreCase)
if ($seedOffset -lt 0 -or $registeredPath.IndexOf($seedArgument, $seedOffset + $seedArgument.Length, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) {
    throw "Could not uniquely locate the seed argument in SCM ImagePath; protected seed remains at '$seedStage'."
}
$seedlessPath = $registeredPath.Remove($seedOffset, $seedArgument.Length).Trim()
$change = Invoke-CimMethod -InputObject $adbService -MethodName Change -Arguments @{ PathName = $seedlessPath }
if ($change.ReturnValue -ne 0) { throw "Could not detach the seed from SCM ImagePath: $($change.ReturnValue); protected seed remains at '$seedStage'." }
$detachedService = Get-CimInstance -ClassName Win32_Service -Filter "Name='DroidHarborADB'"
if ($null -eq $detachedService -or $detachedService.PathName.Contains('--auth-seed-dir')) {
    throw "SCM ImagePath still references the protected seed; retained at '$seedStage'."
}
Remove-Item -LiteralPath $seedStage -Recurse -Force
New-Item -ItemType Directory -Path $PlatformDataTarget, $ConfigTarget | Out-Null
Copy-Tree (Join-Path $BackupSource 'platform-data') $PlatformDataTarget
Copy-Tree (Join-Path $BackupSource 'config') $ConfigTarget
& $DroidHarborBinary migrate --database (Join-Path $PlatformDataTarget 'droidharbor.db') | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Restored database version/schema validation failed; services remain stopped.' }
& sc.exe config DroidHarborADB start= auto | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not restore ADB automatic startup; services remain stopped.' }
& sc.exe config DroidHarbor start= auto | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not restore platform automatic startup; services remain stopped.' }
Write-Output 'Restore copied platform state/config and verified the exact backup ADB authorization in the actual Windows service-account profile.'
Write-Output 'The protected seed was detached from SCM only after ADB readiness and key verification. Services remain stopped; boot recovery does not resume old leases.'
