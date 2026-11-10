[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BinaryPath,
    [Parameter(Mandatory=$true)][string]$DataDirectory,
    [Parameter(Mandatory=$true)][string]$ConfigDirectory,
    [Parameter(Mandatory=$true)][string]$AdbExecutable,
    [Parameter(Mandatory=$true)][string]$AdbListen,
    [Parameter(Mandatory=$true)][string]$TransferDirectory,
    [string]$AdbAuthorizationSeedDirectory,
    [switch]$NoStart
)
$ErrorActionPreference = 'Stop'
$platformName = 'DroidHarbor'
$adbName = 'DroidHarborADB'
$platformAccount = "NT SERVICE\$platformName"
$adbAccount = "NT SERVICE\$adbName"
$created = [System.Collections.Generic.List[string]]::new()
$seedStage = $null

function Require-File([string]$Path, [string]$Name) {
    if ($Path -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "$Name must be an existing absolute file path."
    }
}
function Require-Directory([string]$Path, [string]$Name) {
    if ($Path -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or -not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "$Name must be an existing absolute directory path."
    }
}
function Quote-WindowsArgument([string]$Value) {
    if ($Value.Contains('"')) { throw 'Service command arguments may not contain quote characters.' }
    $trailing = 0
    for ($index = $Value.Length - 1; $index -ge 0 -and $Value[$index] -eq [char]92; $index--) { $trailing++ }
    $prefix = $Value.Substring(0, $Value.Length - $trailing)
    return '"' + $prefix + ('\' * ($trailing * 2)) + '"'
}
function Invoke-Sc([string[]]$Arguments) {
    $output = & sc.exe @Arguments
    if ($LASTEXITCODE -ne 0) { throw "SCM '$($Arguments[0])' failed with exit code $LASTEXITCODE. $($output -join ' ')" }
}
function Grant-Tree([string]$Path, [string]$Account, [string]$Rights) {
    & icacls.exe $Path /grant "${Account}:$Rights" /T /C /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not apply required ACLs to '$Path'." }
}
function Grant-File([string]$Path, [string]$Account, [string]$Rights) {
    & icacls.exe $Path /grant "${Account}:$Rights" /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not apply required ACLs to '$Path'." }
}
function Grant-Directory([string]$Path, [string]$Account, [string]$Rights) {
    & icacls.exe $Path /grant "${Account}:$Rights" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not apply required ACLs to '$Path'." }
}
function Wait-Service([string]$Name, [string]$State) {
    $target = [System.Enum]::Parse([System.ServiceProcess.ServiceControllerStatus], $State)
    $service = Get-Service -Name $Name -ErrorAction Stop
    $service.WaitForStatus($target, [TimeSpan]::FromSeconds(45))
    $service.Refresh()
    if ($service.Status -ne $target) { throw "Service '$Name' did not reach '$State'." }
}
function Get-TreeManifest([string]$Path) {
    $root = [System.IO.Path]::GetFullPath($Path).TrimEnd('\') + '\'
    $files = @(Get-ChildItem -LiteralPath $Path -File -Force -Recurse | ForEach-Object {
        $relative = $_.FullName.Substring($root.Length)
        "$relative`0$((Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash)"
    } | Sort-Object)
    return $files -join "`n"
}
function Get-AdbProfileAndroidDirectory {
    $serviceAccount = New-Object -TypeName System.Security.Principal.NTAccount -ArgumentList 'NT SERVICE\DroidHarborADB'
    $sid = $serviceAccount.Translate([System.Security.Principal.SecurityIdentifier]).Value
    $profiles = @(Get-CimInstance -ClassName Win32_UserProfile -Filter "SID='$sid'")
    if ($profiles.Count -ne 1 -or [string]::IsNullOrWhiteSpace($profiles[0].LocalPath)) {
        throw 'Windows has no unique registered profile for the DroidHarborADB service account.'
    }
    return Join-Path ([System.IO.Path]::GetFullPath($profiles[0].LocalPath)) '.android'
}

try {
    Require-File $BinaryPath 'BinaryPath'
    Require-File $AdbExecutable 'AdbExecutable'
    Require-Directory $ConfigDirectory 'ConfigDirectory'
    Require-File (Join-Path $ConfigDirectory '.env') '.env'
    if ([string]::IsNullOrWhiteSpace($AdbListen)) {
        throw 'The dedicated ADB listen address is required.'
    }
    if ($DataDirectory -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})' -or $TransferDirectory -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2})') {
        throw 'DataDirectory and TransferDirectory must be absolute paths.'
    }
    $root = [System.IO.Path]::GetFullPath($DataDirectory).TrimEnd('\') + '\'
    $transfer = [System.IO.Path]::GetFullPath($TransferDirectory)
    if (-not $transfer.StartsWith($root, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'TransferDirectory must be inside DataDirectory.'
    }
    foreach ($name in @($platformName, $adbName)) {
        if (Get-Service -Name $name -ErrorAction SilentlyContinue) { throw "Service '$name' already exists; refusing replacement." }
    }
    if (-not [string]::IsNullOrWhiteSpace($AdbAuthorizationSeedDirectory)) {
        Require-Directory $AdbAuthorizationSeedDirectory 'AdbAuthorizationSeedDirectory'
        if (-not (Test-Path -LiteralPath (Join-Path $AdbAuthorizationSeedDirectory 'adbkey') -PathType Leaf)) {
            throw 'AdbAuthorizationSeedDirectory must contain the official adbkey private key.'
        }
        $stageRoot = Join-Path $env:ProgramData 'DroidHarbor\staging'
        New-Item -ItemType Directory -Path $stageRoot -Force | Out-Null
        $seedStage = Join-Path $stageRoot ('adb-auth-' + [Guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $seedStage | Out-Null
        & icacls.exe $seedStage /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' /Q | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not restrict temporary ADB authorization seed '$seedStage'." }
        & robocopy.exe $AdbAuthorizationSeedDirectory $seedStage /E /COPY:DAT /DCOPY:DAT /XJ /R:1 /W:1 /NFL /NDL /NJH /NJS /NP | Out-Null
        if ($LASTEXITCODE -ge 8) { throw "Could not copy ADB authorization seed into protected staging directory '$seedStage'." }
    }
    New-Item -ItemType Directory -Path $DataDirectory -Force | Out-Null
    New-Item -ItemType Directory -Path $TransferDirectory -Force | Out-Null
    $logRoot = Join-Path $env:ProgramData 'DroidHarbor\logs'
    $platformLogDirectory = Join-Path $logRoot 'platform'
    $adbLogDirectory = Join-Path $logRoot 'adb'
    New-Item -ItemType Directory -Path $platformLogDirectory, $adbLogDirectory -Force | Out-Null

    # The platform reads its runtime settings from one dotenv file.
    # Its database/admin/transfer paths must point to the directories granted below.
    $serveArgs = @('serve', '--env-file', (Join-Path $ConfigDirectory '.env'))
    $adbArgsWithoutSeed = @('adb-serve', '--adb', $AdbExecutable, '--listen', $AdbListen, '--log-file', (Join-Path $adbLogDirectory 'adb.jsonl'))
    $adbArgs = @($adbArgsWithoutSeed)
    if ($seedStage) { $adbArgs += @('--auth-seed-dir', $seedStage) }
    $binary = Quote-WindowsArgument $BinaryPath
    $platformCommand = $binary + ' ' + (($serveArgs | ForEach-Object { Quote-WindowsArgument $_ }) -join ' ')
    $adbCommandWithoutSeed = $binary + ' ' + (($adbArgsWithoutSeed | ForEach-Object { Quote-WindowsArgument $_ }) -join ' ')
    $adbCommand = $binary + ' ' + (($adbArgs | ForEach-Object { Quote-WindowsArgument $_ }) -join ' ')

    New-Service -Name $adbName -DisplayName $adbName -BinaryPathName $adbCommand -StartupType Manual | Out-Null
    $created.Add($adbName)
    New-Service -Name $platformName -DisplayName $platformName -BinaryPathName $platformCommand -StartupType Manual | Out-Null
    $created.Add($platformName)
    # Virtual service identities become resolvable only after registration.
    # Both services remain stopped until their dedicated accounts and ACLs are set.
    foreach ($name in @($adbName, $platformName)) {
        $configuration = Get-CimInstance -ClassName Win32_Service -Filter "Name='$name'"
        $change = Invoke-CimMethod -InputObject $configuration -MethodName Change -Arguments @{ StartName = "NT SERVICE\$name"; StartPassword = $null }
        if ($change.ReturnValue -ne 0) { throw "Could not assign dedicated account to '$name': $($change.ReturnValue)." }
    }
    if ($seedStage) { Grant-Tree $seedStage $adbAccount '(OI)(CI)R' }
    Invoke-Sc @('description', $platformName, 'DroidHarbor HTTP device-management platform.')
    Invoke-Sc @('description', $adbName, 'Dedicated DroidHarbor official ADB server.')
    Invoke-Sc @('config', $platformName, 'depend=', $adbName)
    foreach ($name in @($platformName, $adbName)) {
        Invoke-Sc @('failure', $name, 'reset=', '86400', 'actions=', 'restart/5000/restart/30000/restart/60000')
        Invoke-Sc @('failureflag', $name, '1')
    }

    Grant-File $BinaryPath $platformAccount 'RX'
    Grant-File $BinaryPath $adbAccount 'RX'
    Grant-Tree $ConfigDirectory $platformAccount '(OI)(CI)R'
    Grant-Tree $DataDirectory $platformAccount '(OI)(CI)M'
    Grant-Tree $platformLogDirectory $platformAccount '(OI)(CI)M'
    Grant-Tree $adbLogDirectory $adbAccount '(OI)(CI)M'
    Grant-File $AdbExecutable $adbAccount 'RX'
    Grant-Directory (Split-Path -Parent $BinaryPath) $platformAccount '(RX)'
    Grant-Directory (Split-Path -Parent $BinaryPath) $adbAccount '(RX)'
    Grant-Directory (Split-Path -Parent $AdbExecutable) $adbAccount '(RX)'

    $serviceStartType = if ($NoStart) { 'demand' } else { 'auto' }
    Invoke-Sc @('config', $adbName, 'start=', $serviceStartType)
    Invoke-Sc @('config', $platformName, 'start=', $serviceStartType)
    if ($NoStart) {
        if ($seedStage) {
            Write-Output "Services are registered but stopped. Restore must start DroidHarborADB once; protected authorization seed remains at '$seedStage'."
        } else {
            Write-Output 'Services are registered but stopped. No ADB authorization seed was configured.'
        }
    } else {
        Start-Service -Name $adbName
        Wait-Service $adbName 'Running'
        if ($seedStage) {
            $actualAndroid = Get-AdbProfileAndroidDirectory
            if ((Get-TreeManifest $actualAndroid) -ne (Get-TreeManifest $seedStage)) {
                throw "ADB readiness succeeded but the service profile does not exactly match protected seed '$seedStage'; seed and SCM ImagePath are retained."
            }
            $serviceConfiguration = Get-CimInstance -ClassName Win32_Service -Filter "Name='$adbName'"
            $change = Invoke-CimMethod -InputObject $serviceConfiguration -MethodName Change -Arguments @{ PathName = $adbCommandWithoutSeed }
            if ($change.ReturnValue -ne 0) { throw "Could not detach the protected seed from SCM ImagePath: $($change.ReturnValue)." }
            $registered = Get-CimInstance -ClassName Win32_Service -Filter "Name='$adbName'"
            if ($null -eq $registered -or $registered.PathName.Contains('--auth-seed-dir')) {
                throw "Could not detach the protected seed from the SCM ImagePath; seed remains at '$seedStage'."
            }
            Remove-Item -LiteralPath $seedStage -Recurse -Force
            $seedStage = $null
        }
        Start-Service -Name $platformName
        Wait-Service $platformName 'Running'
        Write-Output 'DroidHarbor services installed and reported Running after listener readiness.'
    }
}
catch {
    foreach ($name in @($platformName, $adbName)) {
        if ($created.Contains($name)) {
            $service = Get-Service -Name $name -ErrorAction SilentlyContinue
            if ($null -ne $service -and $service.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped) {
                Stop-Service -Name $name -Force -ErrorAction SilentlyContinue
                $service = Get-Service -Name $name -ErrorAction SilentlyContinue
                if ($null -ne $service) {
                    $service.WaitForStatus([System.ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(45))
                }
            }
            & sc.exe delete $name | Out-Null
        }
    }
    if ($seedStage -and (Test-Path -LiteralPath $seedStage)) {
        Write-Warning "Protected ADB authorization seed retained at '$seedStage'; service registration was rolled back. Correct the failure and rerun installation/restore explicitly."
    }
    throw
}
