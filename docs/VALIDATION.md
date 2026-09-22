# Validation status

This file records actual validation, not a claim that every environment is covered.

## Completed checks — 2026-09-22

| Check | Result |
| --- | --- |
| `go test ./... -timeout 6m` | All packages passed on Windows amd64 |
| `go vet ./...` | Passed |
| `go run ./cmd/release` | Tests/vet and all three release builds passed |
| windows/amd64, darwin/arm64, darwin/amd64 | Binaries produced; SHA256 manifest verified |
| Native `aih version`, help and `demo` | Passed |
| Native `aih doctor --provider claude-code` | Git/gh present, Claude CLI capabilities and GitHub authentication confirmed |
| Windows source installer | Fresh install, execution and replacement passed; prior binary retained |
| PowerShell / shell installer and release syntax | Passed |
| Staged Git contents and credential-pattern scan | No runtime databases/logs/env files/binaries or credential/local-root matches |

The final demo test completed in about 139 seconds. It asserted three serialized
merges with distinct current-base evidence and independent reviews, a persistent
human blocker, and reconstruction after deletion of the complete local project.
Additional integration tests cover interrupted task graphs, live/stale controller
leases, rejected atomic transactions, conflict resolution, and post-merge holds.
Native process tests cover prompt input, timeouts, exclusive locks and killing
descendants after supervisor death. Provider and GitHub protocol tests use mock
executables; update tests cover version compatibility and checksum tampering.

## Current boundaries

- Native development machine: Windows amd64.
- Codex/Claude adapters are tested with executable protocol fixtures, including
  malformed results, crashes and timeouts. Live model jobs are not used in tests.
- Claude CLI capability help was inspected locally; Codex was not installed in the
  development PATH. Its sandbox/structured-output contract was checked against
  [OpenAI's noninteractive documentation](https://learn.chatgpt.com/docs/non-interactive-mode).
- macOS binaries are cross-compiled. Native macOS execution remains unverified here.
- Demo GitHub calls are fake; Git operations and SQLite are real.
- No production credentials or paid provider calls are needed for the demo.
- Release artifacts were built locally; a tagged GitHub binary release was not
  published as part of the initial source-repository delivery. Use source install.
- A passing secret-pattern scan is not a guarantee that arbitrary secrets can be
  identified. The trusted-repository security boundary still applies.

## Recommended next validation

Run the test suite and demo on both supported macOS architectures. Then initialize
a small private or public throwaway GitHub application with each supported provider
and complete a bounded real feature. Validate provider authentication, local sandbox
permissions, native application checks and GitHub PR merge recognition for that
installation. Keep production repositories out of the first live trial.
