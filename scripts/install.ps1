param(
    [string]$Source,
    [string]$Version = '1.0.0',
    [string]$Directory = (Join-Path $env:LOCALAPPDATA 'Programs\AIH'),
    [string]$Repository = $(if ($env:AIH_RELEASE_REPO) { $env:AIH_RELEASE_REPO } else { 'nvrakesh06/ai-agentic-harness' })
)
$ErrorActionPreference = 'Stop'
if ($Repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' -or $Repository.Contains('..')) { throw 'Repository must be owner/repository.' }
if ($Version -notmatch '^1\.\d+\.\d+$') { throw 'Install a stable V1 version. Major updates require explicit migration.' }
if (Get-Process -Name aih -ErrorAction SilentlyContinue) { throw 'Stop all AIH supervisors before installing.' }
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('aih-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null
$pending = $null
try {
    if (!$Source) {
        $asset = 'aih_windows_amd64.exe'
        $base = "https://github.com/$Repository/releases/download/v$Version"
        $Source = Join-Path $scratch $asset
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $Source
        $manifest = Join-Path $scratch 'SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $manifest
        $line = Get-Content -LiteralPath $manifest | Where-Object { $_ -match '^[a-fA-F0-9]{64}\s+aih_windows_amd64\.exe$' }
        if (@($line).Count -ne 1) { throw 'Missing or ambiguous release checksum.' }
        $expected = ($line -split '\s+')[0]
        if ((Get-FileHash -LiteralPath $Source -Algorithm SHA256).Hash -ne $expected) { throw 'Release checksum mismatch.' }
    }
    $Source = (Resolve-Path -LiteralPath $Source).Path
    $destination = [IO.Path]::GetFullPath($Directory)
    New-Item -ItemType Directory -Force -Path $destination | Out-Null
    $target = Join-Path $destination 'aih.exe'
    $pending = Join-Path $destination ('aih-' + [guid]::NewGuid().ToString('N') + '.new')
    Copy-Item -LiteralPath $Source -Destination $pending
    if (Test-Path -LiteralPath $target) {
        $backup = Join-Path $destination ('aih-' + [guid]::NewGuid().ToString('N') + '.bak')
        [IO.File]::Replace($pending, $target, $backup)
        Write-Host "Previous binary retained at $backup"
    } else { [IO.File]::Move($pending, $target) }
    Write-Host "Installed $target. Add $destination to your user PATH, then run aih install."
} finally {
    # Only the unique adjacent temporary file created by this invocation.
    if ($pending -and (Test-Path -LiteralPath $pending)) {
        Remove-Item -LiteralPath $pending -Force
    }
    $resolvedScratch = [IO.Path]::GetFullPath($scratch)
    $tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if ($resolvedScratch.StartsWith($tempRoot, [StringComparison]::OrdinalIgnoreCase) -and
        [IO.Path]::GetFileName($resolvedScratch).StartsWith('aih-install-')) {
        Remove-Item -LiteralPath $resolvedScratch -Recurse -Force
    }
}
