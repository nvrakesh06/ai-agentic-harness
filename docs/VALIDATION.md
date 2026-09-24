# Validation record

These are observed results from **2026-09-22**, not a claim that every platform,
provider account or cloud image is covered. The readiness work preserves the V1
state schema and was tested on native Windows amd64 and Ubuntu 24.04 amd64.

## Completed checks

| Check | Result |
| --- | --- |
| Complete Go tests, Windows | Passed in the fresh candidate clone, uncached; all 14 test packages |
| Complete Go tests, Ubuntu | Passed in the fresh candidate clone, uncached; all 14 test packages |
| `go test -race -p 1 ./... -count=1 -timeout 10m`, Ubuntu | Passed for the entire frozen candidate |
| `go vet ./...`, formatting lint | Passed on Windows and Ubuntu |
| Release cross-builds | All five targets built independently on Windows and Ubuntu: Windows amd64, macOS amd64/arm64, Linux amd64/arm64 |
| Release artifact checks | Both SHA256 manifests verified and byte-identical across build hosts; Linux amd64 and Windows amd64 binaries started natively |
| Source setup, module verification, binary startup | Passed on Windows and fresh Ubuntu |
| Native CLI demo | Passed on both OSes: three DONE, one human blocker, complete local-project loss and reconstruction |
| Windows doctor | Real Codex 0.155.1 and Claude Code 2.1.278 capabilities/authentication, GitHub auth and SQLite probe passed; no model calls |
| Ubuntu doctor | Missing provider/authentication correctly returned failure with actionable messages; positive/negative protocol fixtures passed |
| Unix/PowerShell installers | Fresh install, executable startup, replacement and backup preservation passed |
| Installer failure paths | Missing source / locked destination failed safely, retained the old binary and cleaned temporary files |
| Release-download installer paths | Offline transport fixtures accepted the real candidate assets/checksums and rejected invalid manifests without replacing the installed binary |
| systemd user-unit generation | Idempotence, private permissions and refusal to overwrite different units passed |
| Ubuntu systemd 255 unit parser | Generated units verified, including spaces, percent signs and dollar signs in paths |
| ShellCheck / PowerShell AST parsing | Passed |
| Markdown file links / Git whitespace / executable script modes | Checked |
| Tracked/current source and reachable-history credential patterns | No obvious secrets, private keys or personal workspace roots found |
| Go vulnerability scan, Linux amd64 | `govulncheck` v1.8.0 reported no vulnerabilities with Go 1.27.1 |

In the release-candidate rerun, Windows package times included demo **160.381s**,
engine **82.299s**, and Git **41.136s**. Ubuntu equivalents were **14.368s**,
**17.468s**, and **4.483s**; its race-instrumented demo/engine took **21.432s** and
**19.409s**. Both native CLI demos also passed independently.

During the earlier readiness phase, a concurrent run on the loaded Windows host
exceeded its five-minute fixture deadline; subsequent native/serialized runs
passed without relaxing assertions or timeouts. Release validation runs uncached
tests. For a constrained development machine,
use `go test -p 1 ./... -count=1 -timeout 15m` rather than running multiple suites
and cross-builds at once. `GOMAXPROCS=2` was also used for the final serialized runs.

The vulnerability scanner must itself use a sufficiently recent toolchain:

```sh
GOTOOLCHAIN=go1.27.1 go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

An initial scanner built automatically with Go 1.26 could not parse the project's
Go 1.27 standard library; rebuilding it with the pinned project toolchain passed.
A clean scan is time/platform-specific, not a security certification. See
[Go's vulnerability-checking guidance](https://go.dev/doc/security/vuln/).

## Fresh-machine simulation

The final audit staged only intended files and exported frozen Git tree
`fcc98b8083619db81b0e76156b65895b8e432090` using `git archive`. It created **no
candidate commit, tag, push or release**. A fresh local Windows clone and a fresh
public clone in isolated `ubuntu:24.04` both received that archive; staging in
each disposable clone and running `git write-tree` reproduced the exact tree ID.
The base commit was `03926d909bb118280a1044744316d642cbc44da0`. Only audit/release
documentation was updated after those tests; executable source, tests, module
files, setup scripts and installers remain byte-identical to the tested tree.

Ubuntu used a dedicated non-root `aih` account and the documented apt prerequisites,
plus GCC and ShellCheck for validation. There were **no mounted/copied laptop
provider credentials or Go caches**. Packaged Go **1.22.2** downloaded Go **1.27.1**
and modules, verified them, and built the binary with `setup.sh`. Windows used
its installed Go **1.21.4** bootstrap with the selected Go 1.27.1 and an isolated,
reused build cache. Both setup/install/doctor wrappers were exercised natively.
An Ubuntu package-mirror checksum mismatch resolved on a no-cache retry; checksum
and signature verification were never disabled.

Exact core validation commands (with `GOMAXPROCS=2`, `GOFLAGS=-p=1`):

```sh
go run ./cmd/checkfmt
go run ./cmd/release  # invoked through each OS's release wrapper
# release runs: go test -p=1 ./... -count=1 -timeout 15m; go vet ./...
# and five CGO-disabled, trimmed-path cross-builds with SHA256SUMS
go test -race -p 1 ./... -count=1 -timeout 10m  # Ubuntu with GCC
GOTOOLCHAIN=go1.27.1 go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
shellcheck scripts/*.sh  # Ubuntu; PowerShell AST checks ran on Windows
```

Release-download fixtures replaced only HTTP transport with local candidate
assets; no unpublished GitHub release was assumed to exist. Real release hosting,
download permissions and clean-tag VCS metadata must be checked when publishing.

The final test container used Docker's `--init` so orphaned test descendants were
reaped. During the earlier readiness phase, a Docker backend interruption required
restarting the disposable environment; subsequent complete race runs passed.
Docker is a test environment here, **not a deployment requirement**.

No real systemd PID 1/user manager ran in the container. Unit verification used
`systemd-analyze --user verify` with a private temporary `XDG_RUNTIME_DIR`; its
missing-system-bus warning is expected there. This validates unit syntax and
executable paths, **not** login/linger, a cloud reboot, or journal retention.

## Recovery and concurrency coverage

- A real supervisor subprocess is killed, an immediate restart refuses its live
  remote lease, and a later restart after simulated test-lease expiry acquires a
  new epoch and consumes a persisted command exactly once. Orderly handoff then
  releases ownership. This runs on Windows and Linux without provider calls.
- Existing integration tests cover interrupted task graphs, running/verification
  recovery, stale/live leases, atomic publication rejection, conflict resolution,
  post-merge holds and complete local-cache reconstruction.
- Native process tests cover stdin, timeouts, exclusive locks, cancellation and
  descendant termination after supervisor death.
- Linux testing exposed a local Git tracking-ref race between push and fetch.
  Explicit fetch ownership and a cross-process local fetch lock fixed it; the
  deterministic regression and three consecutive Linux demos passed. Remote
  expected-ref fencing was not weakened.
- Native systemd validation caught separate quoting rules for WorkingDirectory
  and the executable name. Corrected rendering has regression tests and passed
  the real parser with ordinary and special-character paths.

## Remaining acceptance checks

- Boot an actual cloud VM, authenticate as its application user, enable linger,
  start the generated unit, disconnect SSH, reboot and verify state/log retention.
- Run one small bounded live task in a disposable GitHub application with each
  selected provider. Mock providers/GitHub exercise protocols, while Git and SQLite
  are real; tests do not establish billing/quota, provider sandbox viability,
  model entitlement, organization permissions or real PR recognition.
- Native macOS and Linux arm64 execution remain unverified. Cross-compilation
  alone is not runtime validation.
- No tagged binary release or readiness commit was published by this phase.
  Build from source until release assets exist. Review/commit/push the local
  changes before expecting a new public clone to contain them.

The repository was already public; private vulnerability reporting is now enabled.
Do not run untrusted repositories or treat passing tests/scanners as hostile-code
isolation. See [SECURITY.md](../SECURITY.md) and [VM setup](VM_SETUP.md).
