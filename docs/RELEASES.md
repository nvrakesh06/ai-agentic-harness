# Releases and updates

From the repository root, run `go run ./cmd/release`, `sh scripts/release.sh`, or
`./scripts/release.ps1`. This runs uncached tests/vet, builds with CGO disabled and trimmed
paths, then writes:

```text
dist/aih_windows_amd64.exe
dist/aih_darwin_arm64
dist/aih_darwin_amd64
dist/aih_linux_amd64
dist/aih_linux_arm64
dist/SHA256SUMS
```

Publishing is explicit. Set the version in `internal/model`, update release notes,
commit, tag `v1.x.y`, and push that tag. On a clean tagged HEAD, run
`go run ./cmd/release -publish` (PowerShell wrapper: `-Publish`). The command uses
`gh release create --verify-tag`; it does not create a tag from arbitrary main.
There is no CI/Actions prerequisite and normal builds make no GitHub release writes.
`-repo owner/repository` or `AIH_RELEASE_REPO` selects a fork's release destination.
Release installers support Linux amd64/arm64 as well as the original targets;
Go is not required for an installed release binary. Until a tag has actual release
assets, use source setup. A version constant alone does not mean a release exists.

The initial release version is **1.0.0**, with tag **v1.0.0**. Installers and the
updater accept stable `1.x.y` versions only, not `-rc` prereleases. Validate a
candidate from source before tagging. If committing is not yet authorized, export
the staged tree with `git write-tree` / `git archive` and verify its tree identity
in a disposable clone; this creates no candidate commit. After approval, rebuild
from the clean tagged commit: candidate binaries carry dirty/base-commit VCS
metadata and are not the final release assets.

## Update behavior

Start/attach check for newer non-draft, non-prerelease V1 releases unless disabled.
An unavailable update service is a warning, not a workflow blocker. `aih self-update`
downloads the matching OS/architecture asset and SHA256SUMS into a new cache
directory, verifies the digest and prints the staged path. It never changes the
current executable or running supervisor version.

`AIH_RELEASE_REPO` overrides all release lookups. Without it, startup notifications
use canonical project `release_repo`, while standalone `self-update` and installers
use this harness's upstream. Set the environment override when consuming a fork.

After stopping **all** supervisors, use `scripts/install.ps1 -Source PATH` or
`sh scripts/install.sh --source PATH`. The installers copy to a temporary adjacent
file before replacement and retain the previous binary. A local `--source` binary
is trusted by the caller; downloaded releases are checksum-verified. PATH changes
are printed, not silently applied to the user's shell configuration.

Checksums detect corruption, not compromise of the release account. V1 does not
provide signed provenance, Windows Authenticode, macOS notarization or automatic
major migrations. These are release-hardening follow-ups. Never disable platform
security globally to install a release; inspect the artifact and follow your
organization's approval process.
