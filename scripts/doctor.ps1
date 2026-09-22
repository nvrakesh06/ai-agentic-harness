$ErrorActionPreference = 'Stop'
$binary = Join-Path $PSScriptRoot '..\bin\aih.exe'
if (!(Test-Path -LiteralPath $binary)) { throw 'Build first with scripts/setup.ps1, or use an installed aih doctor.' }
& $binary doctor @args
exit $LASTEXITCODE
