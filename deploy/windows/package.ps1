[CmdletBinding()]
param(
    [string]$OutputDirectory,
    [string]$GoExecutable
)
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $root ('release/windows-amd64-' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $OutputDirectory) { throw "Output already exists: $OutputDirectory" }
if (-not $GoExecutable) { $GoExecutable = Join-Path $root '.tools/go/bin/go.exe' }
if (-not (Test-Path -LiteralPath $GoExecutable -PathType Leaf)) { throw "Go executable not found: $GoExecutable" }
$serverExecutable = Join-Path $OutputDirectory 'droidharbor.exe'
$stagingDirectory = Join-Path $OutputDirectory '.skill-package'
$skillDirectory = Join-Path $stagingDirectory 'droidharbor'
$windowsDirectory = Join-Path $skillDirectory 'scripts/windows-amd64'
$linuxDirectory = Join-Path $skillDirectory 'scripts/linux-amd64'
$skillArchive = Join-Path $OutputDirectory 'DroidHarbor-skill.zip'
$oldGoOS = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCGO = $env:CGO_ENABLED
New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
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
    Write-Output "Server executable: $serverExecutable"
    Write-Output "Client Skill: $skillArchive"
} finally {
    try {
        if (Test-Path -LiteralPath $stagingDirectory) { Remove-Item -LiteralPath $stagingDirectory -Recurse -Force }
    } finally {
        $env:GOOS = $oldGoOS; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCGO
        Pop-Location
    }
}
