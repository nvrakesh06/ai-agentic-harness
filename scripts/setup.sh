#!/bin/sh
# Build only this repository's pinned toolchain/modules. Never sudo or authenticate.
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_root"
case "$(uname -s)" in Linux|Darwin) ;; *) echo 'Use scripts/setup.ps1 on Windows.' >&2; exit 1 ;; esac
for tool in git go; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "Missing $tool. Ubuntu: sudo apt-get install git golang-go ca-certificates" >&2
        echo 'Other systems: install Git and Go 1.21+ from their official installers.' >&2
        exit 1
    }
done
echo 'Building AIH. Go may download the toolchain pinned in go.mod and checksummed modules.'
echo 'No agent CLI, authentication, system packages, shell profile, or Git identity will be changed.'
go version
go mod download
go mod verify
mkdir -p bin
go build -trimpath -o bin/aih ./cmd/aih
./bin/aih version
echo 'Built ./bin/aih. Try ./bin/aih demo (no credentials or model charges).'
echo 'Optional PATH install: sh scripts/install.sh --source ./bin/aih'
echo 'Next: docs/AGENTS_SETUP.md, then ./scripts/doctor.sh --machine --provider codex'
