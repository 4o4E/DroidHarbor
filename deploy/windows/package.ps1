[CmdletBinding()]
param(
    [string]$OutputDirectory,
    [string]$GoExecutable
)
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $root ('release/DroidHarbor-windows-amd64-' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $OutputDirectory) { throw "Output already exists: $OutputDirectory" }
if (-not $GoExecutable) { $GoExecutable = Join-Path $root '.tools/go/bin/go.exe' }
if (-not (Test-Path -LiteralPath $GoExecutable -PathType Leaf)) { throw "Go executable not found: $GoExecutable" }
$oldGoOS = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCGO = $env:CGO_ENABLED
Push-Location $root
try {
    & npm.cmd --prefix web ci --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed: $LASTEXITCODE" }
    & npm.cmd run web:build
    if ($LASTEXITCODE -ne 0) { throw "web build failed: $LASTEXITCODE" }
    & npm.cmd run assets:prepare
    if ($LASTEXITCODE -ne 0) { throw "asset preparation failed: $LASTEXITCODE" }
    New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $OutputDirectory 'droidharbor.exe') ./cmd/droidharbor
    if ($LASTEXITCODE -ne 0) { throw "server build failed: $LASTEXITCODE" }
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $OutputDirectory 'droidharbor-cli.exe') ./cmd/droidharbor-cli
    if ($LASTEXITCODE -ne 0) { throw "CLI build failed: $LASTEXITCODE" }
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'start.cmd'), (Join-Path $PSScriptRoot 'admin-init.cmd'), (Join-Path $PSScriptRoot 'README.md') -Destination $OutputDirectory
    Copy-Item -LiteralPath (Join-Path $root 'skills/droidharbor/SKILL.md') -Destination (Join-Path $OutputDirectory 'SKILL.md')
    & (Join-Path $OutputDirectory 'droidharbor.exe') version
    if ($LASTEXITCODE -ne 0) { throw "server version check failed: $LASTEXITCODE" }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $archive = $OutputDirectory + '.zip'
    [IO.Compression.ZipFile]::CreateFromDirectory($OutputDirectory, $archive)
    Write-Output "Windows package: $OutputDirectory"
    Write-Output "Windows archive: $archive"
} finally {
    $env:GOOS = $oldGoOS; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCGO
    Pop-Location
}
