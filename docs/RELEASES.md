# Releases and updates

From the repository root, run `go run ./cmd/release`, `sh scripts/release.sh`, or
`./scripts/release.ps1`. This runs tests/vet, builds with CGO disabled and trimmed
paths, then writes:

```text
dist/aih_windows_amd64.exe
dist/aih_darwin_arm64
dist/aih_darwin_amd64
dist/SHA256SUMS
```

Publishing is explicit. Set the version in `internal/model`, update release notes,
commit, tag `v1.x.y`, and push that tag. On a clean tagged HEAD, run
`go run ./cmd/release -publish` (PowerShell wrapper: `-Publish`). The command uses
`gh release create --verify-tag`; it does not create a tag from arbitrary main.
There is no CI/Actions prerequisite and normal builds make no GitHub release writes.

## Update behavior

Start/attach check for newer non-draft, non-prerelease V1 releases unless disabled.
An unavailable update service is a warning, not a workflow blocker. `aih self-update`
downloads the matching OS/architecture asset and SHA256SUMS into a new cache
directory, verifies the digest and prints the staged path. It never changes the
current executable or running supervisor version.

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
