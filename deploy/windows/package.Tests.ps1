$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'package.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw $parseErrors[0] }
$publisher = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Publish-ReleaseArtifacts' }, $true)
if (-not $publisher) { throw 'Release publisher not found' }
Invoke-Expression $publisher.Extent.Text
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('DroidHarbor-publish-test-' + [guid]::NewGuid().ToString('N'))
try {
    foreach ($mode in @('locked-current', 'locked-staged', 'success')) {
        $build = Join-Path $fixture ($mode + '/build')
        $output = Join-Path $fixture ($mode + '/release')
        New-Item -ItemType Directory -Path $build, $output | Out-Null
        [IO.File]::WriteAllText((Join-Path $output '.env'), 'runtime-settings')
        foreach ($name in @('droidharbor.exe', 'DroidHarbor-skill.zip')) {
            [IO.File]::WriteAllText((Join-Path $output $name), 'old-' + $name)
            [IO.File]::WriteAllText((Join-Path $build $name), 'new-' + $name)
        }
        $lock = $null
        if ($mode -ne 'success') {
            $lockedPath = if ($mode -eq 'locked-current') { Join-Path $output 'DroidHarbor-skill.zip' } else { Join-Path $build 'DroidHarbor-skill.zip' }
            $lock = [IO.File]::Open($lockedPath, [IO.FileMode]::Open, [IO.FileAccess]::ReadWrite, [IO.FileShare]::None)
        }
        $failed = $false
        try { Publish-ReleaseArtifacts -BuildDirectory $build -OutputDirectory $output -PreviousVersions @() } catch { $failed = $true } finally { if ($lock) { $lock.Dispose() } }
        if ($failed -ne ($mode -ne 'success')) { throw "Unexpected publication result: $mode" }
        $prefix = if ($mode -eq 'success') { 'new-' } else { 'old-' }
        foreach ($name in @('droidharbor.exe', 'DroidHarbor-skill.zip')) {
            if ([IO.File]::ReadAllText((Join-Path $output $name)) -ne ($prefix + $name)) { throw "Mixed or lost artifact: $mode/$name" }
        }
        if ([IO.File]::ReadAllText((Join-Path $output '.env')) -ne 'runtime-settings') { throw 'Runtime configuration changed' }
        if ($mode -eq 'success' -and (Test-Path -LiteralPath (Join-Path $build '.previous'))) { throw 'Successful publication retained rollback files' }
        Write-Output "PASS $mode"
    }
} finally {
    if (Test-Path -LiteralPath $fixture) { Remove-Item -LiteralPath $fixture -Recurse -Force }
}
