# Development and configuration

Use Go 1.26+ with automatic toolchain download enabled, Git, and the normal Go
module cache. Tests use temporary local repositories and fake provider/GitHub
adapters; no authentication or paid model usage is required for tests.

```sh
go test ./... -timeout 6m
go vet ./...
go build -o bin/aih ./cmd/aih          # use bin/aih.exe on Windows
go run ./cmd/release                 # tests, vet, all three cross-builds, checksums
```

The end-to-end suite performs many real Git operations and can take minutes,
especially with Windows antivirus. Do not replace its durability assertions with
only in-memory mocks. `internal/platform` tests exercise timeout and parent-death
cleanup. Run native tests on macOS before claiming macOS runtime validation.

## Application configuration

`aih init` detects Go test/vet, common npm script names, or cargo test. Review this
proposal: detection is not a complete build-system analysis. Existing AGENTS files
are preserved. Commit changes to main; local edits alone do not become active policy.

Representative `.aih/project.yaml` fields:

```yaml
project_id: generated-stable-project-id
provider: codex
base_branch: main
max_parallel_writers: 3
max_parallel_readers: 2
worker_timeout_seconds: 900
lease_seconds: 180
models:
  orchestrator: strong
  implementer: normal
  reviewer: strong
  qa: normal
  designer: strong
  security: strong
  advisor: strongest
provider_models: {}  # optional capability -> installed-provider model ID
checks:
  - name: tests
    command: [go, test, ./...]
    timeout_seconds: 600
release_repo: nvrakesh06/ai-agentic-harness
```

Checks optionally declare `platforms: [windows]` or `[darwin]`. At least one must
apply. They run in the candidate worktree, with credential-like environment keys
filtered. Output is bounded; failures are redacted before portable recording.
Checks must leave tracked source and unignored files unchanged. Use check-mode
formatters and ignore build outputs in the application itself.

Commands are executable plus argv. On Windows, standard npm/npx/Codex/Claude/pnpm
shims are resolved to their Node entrypoints. Other batch scripts require an
explicit trusted `cmd` or PowerShell check. Prefer portable native executables.
AIH does not silently install application dependencies or run repository hooks.

`.aih/policies.yaml` defaults:

```yaml
implementation_retries: 2
review_fix_cycles: 3
qa_fix_cycles: 3
design_fix_cycles: 3
```

After a budget is exhausted there is one Advisor-assisted final attempt. A human
answer resumes the task without silently wiping its failure history. Twenty-four
in-progress checkpoint slices cause a human blocker to bound non-failing churn.

`.aih/harness.lock` defaults:

```yaml
major: 1
minimum: 1.0.0
engineering_rules_version: 1
state_schema: 1
update_channel: stable
auto_update: notify
```

`auto_update: off` disables startup update checks. V1 uses notify-and-stage, not
live binary replacement. Invalid/unknown settings fail rather than silently doing
something else. Stable identities must never be regenerated when reading config.
Worker context, checks, roles and retry policy use canonical main. Changes to the
supervisor's concurrency capacity or lease timing take effect on a clean restart.

## Extending the implementation

Keep provider flags in `provider`, process ownership in `platform`, and lifecycle
inside `engine`. Add schema tests for portable changes, transaction tests for ref
changes, and recovery tests for new states. Do not introduce a provider plugin
framework, external queue, or worktree-local orchestration database.
