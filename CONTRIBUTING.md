# Contributing

Start with README's source setup and `aih demo`; no provider account is needed
to develop or run tests. Use an issue to discuss changes to orchestration,
durability, permissions, or Git publication before implementing them.

Fork the repository, create a topic branch, and submit a focused pull request.
Include the problem, observable behavior, risks, tests run and platform tested.
Do not bundle unrelated formatting or refactors. Keep generated binaries, runtime
state, credentials, private task data and provider transcripts out of commits.

Before submitting:

```sh
go test ./... -timeout 6m
go run ./cmd/checkfmt
go vet ./...
go run ./cmd/release
```

Format edited Go files with `gofmt -w PATH`. Linux contributors with a C compiler
should also run `go test -race ./... -timeout 10m` and `shellcheck scripts/*.sh`.
PowerShell wrappers should be checked on native Windows. Read
[AGENTS.md](AGENTS.md) and [architecture](docs/ARCHITECTURE.md) before changing
the engine. Use executable provider fixtures, real temporary Git repos, and
recovery tests for important behavior; do not require paid model calls in tests.

State/schema changes need migration and interruption tests. Process changes need
timeout, stdin and descendant-cleanup tests. Never weaken atomic ref fencing or
turn a failed verification into a pass to make a test green. Record native versus
cross-build coverage accurately in [validation](docs/VALIDATION.md).

Bug reports should include AIH/OS/provider CLI versions, a minimal reproducer,
expected/actual results, and **sanitized** diagnostics. Never attach entire
AIH_HOME directories or authentication files. Security issues follow
[SECURITY.md](SECURITY.md), not public issues.

Contributions are under the repository's MIT license. Do not contribute code or
data you do not have permission to share. Be constructive and follow the
[code of conduct](CODE_OF_CONDUCT.md).
