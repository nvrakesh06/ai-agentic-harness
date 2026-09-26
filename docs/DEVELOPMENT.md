# Development and configuration

Use Go 1.21+ to bootstrap the toolchain pinned in go.mod (currently Go 1.27.1;
module minimum 1.26), Git 2.28+, and the normal Go module cache. `scripts/setup.sh`
or `scripts/setup.ps1` downloads/verifies modules and builds AIH without installing
agent CLIs or authenticating. Tests use temporary repositories and fake provider/GitHub
adapters; no authentication or paid model usage is required for tests.

```sh
go test -p 1 ./... -timeout 15m
go vet ./...
go run ./cmd/checkfmt                # formatting lint
go build -o bin/aih ./cmd/aih          # use bin/aih.exe on Windows
go run ./cmd/release                 # tests, vet, five cross-builds, checksums
```

The end-to-end suite performs many real Git operations and can take minutes,
especially with Windows antivirus. Do not replace its durability assertions with
only in-memory mocks. `internal/platform` tests exercise timeout and parent-death
cleanup. On Linux with GCC, also run `go test -race ./... -timeout 10m`.
The release command serializes package workers and holds one shared AIH machine
heavy-check slot, visibly waiting for a consumer project's native check for up to
two minutes before reporting resource contention. On a loaded/constrained machine,
use `go test -p 1 ./... -count=1 -timeout 15m` and avoid several concurrent
validation runs. The fifteen-minute
timeout bounds each package test binary; individual
integration fixtures keep their own shorter context deadlines.
Run native tests on macOS before claiming macOS runtime validation.

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
resources:
  max_heavy_checks: 1
  max_light_checks: 2
scheduling:
  target_active_writers: 2
  underutilization_grace_seconds: 30
  backlog_source: queued_objectives
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
provider_models: {}  # omitted tiers use provider-default; doctor lists affected roles
checks:
  - name: tests
    command: [go, test, ./...]
    class: heavy
    timeout_seconds: 600
# Optional. AIH owns the browser runner when a reviewer requests visual
# evidence that its sandbox cannot capture.
visual_capture:
  # Optional. Runs once in AIH's fresh detached checkout at the reviewed head.
  # It may install or integrity-check the runtime for this capture only.
  prepare: [your-runtime, prepare]
  prepare_timeout_seconds: 180
  # This adapter binds 127.0.0.1:0, uses AIH_VISUAL_TLS_CERT and
  # AIH_VISUAL_TLS_KEY, then prints: AIH_VISUAL_READY https://127.0.0.1:<port>
  server: [node, scripts/aih-visual-server.mjs]
  timeout_seconds: 90
  # Optional. Omit targets for the legacy / at 1280x720 desktop.png capture.
  targets:
    - id: desktop
      path: /
      width: 1280
      height: 720
    - id: settings
      path: /settings
      width: 1280
      height: 720
  # Opt in to reuse only when the complete committed tracked tree is unchanged.
  input_closure:
    version: 1
    runtime_identity: chrome-136-playwright-1.52
    targets:
      - id: desktop
      - id: settings
release_repo: nvrakesh06/ai-agentic-harness
```

`lease_seconds` controls the durable takeover window. AIH derives a local status
pulse of at most 30 seconds and a remote half-life renewal from that one value;
there is no second cadence that can be misconfigured beyond the lease expiry.

Checks optionally declare `platforms: [windows]`, `[darwin]` or `[linux]`. At least one must
apply. They run in the candidate worktree, with credential-like environment keys
filtered. Output is bounded; failures are redacted before portable recording.
Checks declare `class: heavy` or `class: light`; an omitted class is heavy for
safe legacy behavior. Limits in `resources` are portable project ceilings. The
machine's `AIH_HOME/machine.yaml` may set `max_heavy_checks` (default 1), and
heavy checks take a machine-local OS lock under the shared AIH home. Command
timeouts start after a slot is acquired. `status` and `watch` show each queued
or running check and its elapsed wait or run time.
Checks must leave tracked source and unignored files unchanged. Use check-mode
formatters and ignore build outputs in the application itself.

For every successful configured check, AIH records exact-head native evidence:
the check name, executable basename, opaque full-command identifier, exit status,
and a bounded list of recognizable pass counts. It never treats arbitrary child
stdout as authenticated stage evidence and never publishes successful stdout,
command arguments, or environment values. Configure independently executable
checks when separate stage evidence is required; AIH runs and records each check
individually.

`visual_capture` is optional and separate from build/test checks. `prepare` and
`server` are project adapter argv values that AIH runs only from a fresh,
supervisor-owned detached checkout at the pinned task head. `prepare` is optional,
uses its own bounded timeout (or the capture timeout when omitted), and runs once
before all configured targets; it can install or verify the runtime in a private
capture cache. AIH never starts either command in the writer worktree. The adapter must
bind `127.0.0.1:0` itself, serve HTTPS with the fresh paths in
`AIH_VISUAL_TLS_CERT` and `AIH_VISUAL_TLS_KEY`, and print one bounded readiness
line: `AIH_VISUAL_READY https://127.0.0.1:<port>`. AIH pins that certificate,
places an AIH-owned loopback gateway in front of the browser, then launches its
fixed Chrome channel and a fresh browser context/page per target. It blocks redirects,
subresources, WebSockets, and other requests outside the gateway origin, and kills
the complete adapter process tree on completion, timeout, or cancellation. The
project cannot choose browser argv, a profile, CDP endpoint, external URLs, or a
pre-existing listener. Each target path is a same-origin absolute path without a
host, query, or fragment; AIH bounds target count, viewports, aggregate pixels and
local artifact size. AIH writes `manifest.json`, one screenshot per target, and a
bounded redacted network diagnostic log outside source. The log prefixes entries
with target IDs. Capture fails as a unit if any target, dimensions, or manifest
mapping fails, so AIH never seals or caches a partial result. It verifies the
worktree remains clean at the exact head before sealing hashes and gives the
requesting reviewer one exact-head retry. Capture is evidence, never a visual pass.

`input_closure` is an opt-in, fail-closed identity for history-only sync heads.
AIH hashes the complete committed Git tree, the closure version, capture
configuration and target definitions, a declared runtime label, and a fresh
Playwright/Chrome/Node runtime probe. It does not infer a smaller dependency
list: any committed file change, including a loader, lockfile, asset, adapter or
fixture, forces a new capture. A missing closure, incomplete target list, changed
identity or runtime, unavailable source artifact, or invalid seal always runs a
fresh capture. When a closure matches, AIH first
revalidates the old manifest and artifact hashes, then copies those bytes into a
new-head local cache and writes a separate receipt naming the source and new
heads. This reattests artifacts only: it never reuses a review disposition,
clears findings, satisfies a missing target, or waives native checks and the
final exact integrated-head gate.

The browser package is a supervisor capability, not a project dependency. Install
Playwright below `AIH_HOME/tools` outside every target worktree and set
`AIH_PLAYWRIGHT_MODULE` to its absolute `.../node_modules/playwright` directory
before starting the supervisor. `AIH_HOME/tools` itself must be a real child of
the resolved AIH home, not a symlink or junction. AIH resolves both paths and
rejects a missing, relative, project-owned, or symlink-escaping module path; it
never resolves browser code from the reviewed project. For each capture, AIH
binds `PLAYWRIGHT_BROWSERS_PATH` for prepare, adapter, and browser processes to
a private child of that capture's visual cache. It overrides any inherited host
value and removes the browser runtime before retaining the capture evidence.

AIH redacts textual manifest and network diagnostics before saving local visual
evidence. Screenshot pixels are opaque image data: AIH does not perform OCR or
claim to detect secrets rendered in a frame. Visual capture is therefore supported
only for applications whose capture route cannot display sensitive data. Image bytes
remain local to the supervising machine and are never written into portable state;
only artifact names and hashes are checkpointed.

This feature adds portable state schema 4. Before activating a schema-4
supervisor, hand off the schema-3 supervisor, retain the local SQLite database
and `aih-state` checkpoint, build a separate new binary, and resume it once.
Confirm status shows the same tasks, heads, and PRs. Do not restart an older
schema-3 binary after schema-4 state is published. Captures remain local to the
machine; after reconstruction elsewhere, rerun a capture at the same head if
the reviewer needs the images.

AIH runs configured checks on the synchronized task head and again on the exact
merge commit before its atomic publication. After publication, the same controller
confirms that remote `main`, configuration, rules, and recorded integration SHA are
unchanged and reuses the exact-merge evidence. Recovery, controller restart,
changed-main, or changed-configuration paths always run fresh post-merge checks.

Commands are executable plus argv. On Windows, standard npm/npx/Codex/Claude/pnpm
shims are resolved to their Node entrypoints. Other batch scripts require an
explicit trusted `cmd` or PowerShell check. Prefer portable native executables.
AIH does not silently install application dependencies or run repository hooks.

### Dependencies in disposable worktrees

Installing dependencies in the original application clone is not enough: checks
also run in task, integration and post-verify worktrees. Put reproducible setup
inside a trusted, committed check script (for example `scripts/verify.sh`) that
runs `npm ci`, lint/tests/build and leaves only ignored build/dependency outputs.
Use `[sh, scripts/verify.sh]` on Linux/macOS and a corresponding explicit
PowerShell command on Windows when needed. Avoid `npm test` in watch mode.
Go checks can download modules into the service user's cache. For offline builds,
pre-provision all caches/toolchains deliberately.

Do not run checks requiring production credentials. Database-backed application
tests need their own documented disposable database setup; AIH does not provision
those services. Run each configured command manually in a fresh worktree before
submitting paid model work. Doctor checks executable availability, not application
test correctness or per-package/runtime version compatibility.

### Machine environment

See `.env.example`. Only `--env-file PATH` loads it; process variables override
file values, and CLI `--home` overrides `AIH_HOME`. Values are literal with no
shell evaluation/interpolation. Agent paths use `CODEX_BINARY` / `CLAUDE_BINARY`.
Keep machine paths and secrets out of canonical project policy.
`AIH_RELEASE_REPO` overrides update provenance for forks. The default upstream
URL and Go module identity name this public project, not a required login account.

Use `aih doctor --machine` before enabling a project, then `aih doctor` inside the
application. `--offline` skips authentication/remote checks and explicitly reports
that it is not a deployment-readiness pass. The doctor never invokes a model or
runs configured application commands. Server setup is in [VM_SETUP.md](VM_SETUP.md).

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
state_schema: 3
update_channel: stable
auto_update: notify
```

`auto_update: off` disables startup update checks. V1 uses notify-and-stage, not
live binary replacement. Invalid/unknown settings fail rather than silently doing
something else. Stable identities must never be regenerated when reading config.
Worker context, checks, roles and retry policy use canonical main. Changes to the
supervisor's concurrency capacity or lease timing take effect on a clean restart.
The first authorized objective starts without waiting for the scheduling grace;
the grace applies when useful writer capacity later falls below target so transient
completion and review transitions do not immediately trigger more planning.
The writer target is best effort and never bypasses the maximum, dependencies,
conflict domains, leases, or retry blockers. `queued_objectives` means only
objectives explicitly accepted from `aih run` may be pulled to fill idle capacity;
the scheduler never invents scope merely to satisfy the target.

## Extending the implementation

Keep provider flags in `provider`, process ownership in `platform`, and lifecycle
inside `engine`. Add schema tests for portable changes, transaction tests for ref
changes, and recovery tests for new states. Do not introduce a provider plugin
framework, external queue, or worktree-local orchestration database.
