$ErrorActionPreference = 'Stop'
Push-Location (Join-Path $PSScriptRoot '..')
try {
    foreach ($tool in @('git', 'go')) {
        if (!(Get-Command $tool -ErrorAction SilentlyContinue)) { throw "Install $tool from its official installer and reopen PowerShell. See README.md." }
    }
    Write-Host 'Building AIH. Go may download the toolchain pinned in go.mod and checksummed modules.'
    Write-Host 'No agent CLI, authentication, system packages, shell profile, or Git identity will be changed.'
    & go version
    if ($LASTEXITCODE -ne 0) { throw 'Go 1.21+ with automatic toolchain selection is required.' }
    & go mod download
    if ($LASTEXITCODE -ne 0) { throw 'Module/toolchain download failed.' }
    & go mod verify
    if ($LASTEXITCODE -ne 0) { throw 'Module verification failed.' }
    & go build -trimpath -o bin/aih.exe ./cmd/aih
    if ($LASTEXITCODE -ne 0) { throw 'AIH build failed.' }
    & .\bin\aih.exe version
    if ($LASTEXITCODE -ne 0) { throw 'AIH startup failed.' }
    Write-Host 'Built .\bin\aih.exe. Try .\bin\aih.exe demo (no credentials or model charges).'
    Write-Host 'Optional PATH install: .\scripts\install.ps1 -Source .\bin\aih.exe'
    Write-Host 'Next: docs/AGENTS_SETUP.md, then .\scripts\doctor.ps1 --machine --provider codex'
} finally { Pop-Location }
