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
$serverDirectory = Join-Path $OutputDirectory 'DroidHarbor-server-windows-amd64'
$clientDirectory = Join-Path $OutputDirectory 'DroidHarbor-client-windows-amd64'
$oldGoOS = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCGO = $env:CGO_ENABLED
Push-Location $root
try {
    & npm.cmd --prefix web ci --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed: $LASTEXITCODE" }
    & npm.cmd run web:build
    if ($LASTEXITCODE -ne 0) { throw "web build failed: $LASTEXITCODE" }
    & npm.cmd run assets:prepare
    if ($LASTEXITCODE -ne 0) { throw "asset preparation failed: $LASTEXITCODE" }
    New-Item -ItemType Directory -Path $serverDirectory, $clientDirectory | Out-Null
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $serverDirectory 'droidharbor.exe') ./cmd/droidharbor
    if ($LASTEXITCODE -ne 0) { throw "server build failed: $LASTEXITCODE" }
    & $GoExecutable build -trimpath -ldflags '-s -w' -o (Join-Path $clientDirectory 'droidharbor-cli.exe') ./cmd/droidharbor-cli
    if ($LASTEXITCODE -ne 0) { throw "CLI build failed: $LASTEXITCODE" }
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'README.md') -Destination $serverDirectory
    Copy-Item -LiteralPath (Join-Path $root 'skills/droidharbor/SKILL.md') -Destination (Join-Path $clientDirectory 'SKILL.md')
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'README.client.md') -Destination (Join-Path $clientDirectory 'README.md')
    & (Join-Path $serverDirectory 'droidharbor.exe') version
    if ($LASTEXITCODE -ne 0) { throw "server version check failed: $LASTEXITCODE" }
    & (Join-Path $clientDirectory 'droidharbor-cli.exe') version
    if ($LASTEXITCODE -ne 0) { throw "CLI version check failed: $LASTEXITCODE" }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    foreach ($directory in @($serverDirectory, $clientDirectory)) {
        $archive = $directory + '.zip'
        [IO.Compression.ZipFile]::CreateFromDirectory($directory, $archive)
        Write-Output "Windows package: $directory"
        Write-Output "Windows archive: $archive"
    }
} finally {
    $env:GOOS = $oldGoOS; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCGO
    Pop-Location
}
