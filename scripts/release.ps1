param([switch]$Publish)
$ErrorActionPreference = 'Stop'
Push-Location (Join-Path $PSScriptRoot '..')
try {
    if ($Publish) { & go run ./cmd/release -publish } else { & go run ./cmd/release }
    if ($LASTEXITCODE -ne 0) { throw 'Release validation/build failed.' }
} finally { Pop-Location }
