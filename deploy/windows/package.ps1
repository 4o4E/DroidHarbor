[CmdletBinding()]
param(
    [string]$GoExecutable
)
$ErrorActionPreference = 'Stop'

# Keep both previous artifacts until the complete replacement and cleanup commits.
# Temporary rollback files are discarded after success, never retained as releases.
function Publish-ReleaseArtifacts {
    param([string]$BuildDirectory, [string]$OutputDirectory, [IO.DirectoryInfo[]]$PreviousVersions)
    $names = @('droidharbor.exe', 'DroidHarbor-skill.zip')
    foreach ($name in $names) {
        if (-not (Test-Path -LiteralPath (Join-Path $BuildDirectory $name) -PathType Leaf)) { throw "Staged artifact missing: $name" }
    }
    $rollback = Join-Path $BuildDirectory '.previous'
    New-Item -ItemType Directory -Path $rollback | Out-Null
    $saved = @()
    $published = @()
    try {
        foreach ($name in $names) {
            $destination = Join-Path $OutputDirectory $name
            if (Test-Path -LiteralPath $destination) {
                if (-not (Test-Path -LiteralPath $destination -PathType Leaf)) { throw "Artifact destination is not a file: $destination" }
                Move-Item -LiteralPath $destination -Destination $rollback
                $saved += $name
            }
        }
        foreach ($name in $names) {
            Move-Item -LiteralPath (Join-Path $BuildDirectory $name) -Destination $OutputDirectory
            $published += $name
        }
        foreach ($directory in $PreviousVersions) { Remove-Item -LiteralPath $directory.FullName -Recurse -Force }
    } catch {
        $publicationError = $_
        $rollbackErrors = @()
        foreach ($name in $published) {
            try { Remove-Item -LiteralPath (Join-Path $OutputDirectory $name) -Force } catch { $rollbackErrors += $_.Exception }
        }
        foreach ($name in $saved) {
            try { Move-Item -LiteralPath (Join-Path $rollback $name) -Destination $OutputDirectory -Force } catch { $rollbackErrors += $_.Exception }
        }
        if ($rollbackErrors.Count -gt 0) {
            $script:preserveBuildDirectory = $true
            throw [AggregateException]::new("Release rollback failed; recovery files kept in $BuildDirectory", [Exception[]](@($publicationError.Exception) + $rollbackErrors))
        }
        throw $publicationError
    }
    Remove-Item -LiteralPath $rollback -Recurse -Force
}

$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
$outputDirectory = Join-Path $root 'release'
$buildDirectory = Join-Path $root ('.workspace/tmp/release-package-' + [guid]::NewGuid().ToString('N'))
if (-not $GoExecutable) { $GoExecutable = Join-Path $root '.tools/go/bin/go.exe' }
if (-not (Test-Path -LiteralPath $GoExecutable -PathType Leaf)) { throw "Go executable not found: $GoExecutable" }
$serverExecutable = Join-Path $buildDirectory 'droidharbor.exe'
$stagingDirectory = Join-Path $buildDirectory '.skill-package'
$skillDirectory = Join-Path $stagingDirectory 'droidharbor'
$windowsDirectory = Join-Path $skillDirectory 'scripts/windows-amd64'
$linuxDirectory = Join-Path $skillDirectory 'scripts/linux-amd64'
$skillArchive = Join-Path $buildDirectory 'DroidHarbor-skill.zip'
$oldGoOS = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCGO = $env:CGO_ENABLED
$preserveBuildDirectory = $false
New-Item -ItemType Directory -Path $buildDirectory | Out-Null
Push-Location $root
try {
    & npm.cmd --prefix web ci --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed: $LASTEXITCODE" }
    & npm.cmd run web:build
    if ($LASTEXITCODE -ne 0) { throw "web build failed: $LASTEXITCODE" }
    & npm.cmd run assets:prepare
    if ($LASTEXITCODE -ne 0) { throw "asset preparation failed: $LASTEXITCODE" }
    New-Item -ItemType Directory -Path $windowsDirectory, $linuxDirectory | Out-Null
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & $GoExecutable build -trimpath -ldflags '-s -w' -o $serverExecutable ./cmd/droidharbor
    if ($LASTEXITCODE -ne 0) { throw "server build failed: $LASTEXITCODE" }
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $windowsDirectory 'droidharbor-cli.exe') ./cmd/droidharbor-cli
    if ($LASTEXITCODE -ne 0) { throw "Windows CLI build failed: $LASTEXITCODE" }
    $env:GOOS = 'linux'
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $linuxDirectory 'droidharbor-cli') ./cmd/droidharbor-cli
    if ($LASTEXITCODE -ne 0) { throw "Linux CLI build failed: $LASTEXITCODE" }
    Copy-Item -LiteralPath (Join-Path $root 'skills/droidharbor/SKILL.md') -Destination (Join-Path $skillDirectory 'SKILL.md')
    & $serverExecutable version
    if ($LASTEXITCODE -ne 0) { throw "server version check failed: $LASTEXITCODE" }
    & (Join-Path $windowsDirectory 'droidharbor-cli.exe') version
    if ($LASTEXITCODE -ne 0) { throw "Windows CLI version check failed: $LASTEXITCODE" }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [IO.Compression.ZipFile]::CreateFromDirectory($stagingDirectory, $skillArchive)
    Remove-Item -LiteralPath $stagingDirectory -Recurse -Force
    New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
    $previousVersions = @(Get-ChildItem -LiteralPath $outputDirectory -Directory -Filter 'windows-amd64-*')
    foreach ($directory in $previousVersions) {
        $otherFiles = @(Get-ChildItem -LiteralPath $directory.FullName -Force | Where-Object {
            $_.PSIsContainer -or $_.Name -notin @('droidharbor.exe', 'DroidHarbor-skill.zip')
        })
        if ($otherFiles.Count -gt 0) { throw "Old release contains non-artifact files; move them before publishing: $($directory.FullName)" }
    }
    Publish-ReleaseArtifacts -BuildDirectory $buildDirectory -OutputDirectory $outputDirectory -PreviousVersions $previousVersions
    Write-Output "Server executable: $(Join-Path $outputDirectory 'droidharbor.exe')"
    Write-Output "Client Skill: $(Join-Path $outputDirectory 'DroidHarbor-skill.zip')"
} finally {
    try {
        if ($preserveBuildDirectory) {
            Write-Warning "Release recovery files retained: $buildDirectory"
        } elseif (Test-Path -LiteralPath $buildDirectory) {
            Remove-Item -LiteralPath $buildDirectory -Recurse -Force
        }
    } finally {
        $env:GOOS = $oldGoOS; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCGO
        Pop-Location
    }
}
