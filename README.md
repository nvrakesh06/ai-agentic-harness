# AI Agentic Harness (AIH)

A local autonomous software-development control plane for **Codex CLI** and
**Claude Code**. Give it an objective; it plans issues, schedules isolated work,
checkpoints code, reviews and tests changes, and integrates verified tasks.
Human decisions go to an inbox without stopping unrelated work.

AIH is not a coding model. It owns lifecycle; models are disposable workers.

```text
Objective -> validated task DAG -> issues -> isolated task worktrees
                                              |
                                      implement / checkpoint
                                              |
                              reviewer + QA + required specialists
                                              |
                                     bounded fix / human inbox
                                              |
                             fresh main -> serialized local verification
                                              |
                         atomic main + task + aih-state publication
                                              |
                                    post-merge verification
```

## Install and try it

Prerequisites: Git, GitHub CLI (`gh auth login`), and an authenticated Codex or
Claude CLI. Building requires Go 1.26+; `go.mod` pins the tested toolchain.
Release binaries do not require Go, SQLite installation, Docker, or a server.

From this checkout, on Windows:

```powershell
go build -o bin/aih.exe ./cmd/aih
.\bin\aih.exe version
.\bin\aih.exe demo
.\scripts\install.ps1 -Source .\bin\aih.exe
```

On macOS:

```sh
make build
./bin/aih demo
sh scripts/install.sh --source ./bin/aih
# Or: make install (installs into Go's binary directory).
```

Add the printed installation directory to your PATH, then `aih install`.
The demo uses temporary local Git repositories and mock model/GitHub adapters:
**no model usage or GitHub writes**. It deliberately removes its disposable
machine-A project directory and reconstructs state on machine B. Other demo
artifacts are retained at the printed path for inspection.

For published tagged binaries, use `scripts/install.ps1 -Version 1.0.0` or
`sh scripts/install.sh --version 1.0.0`; these require that release to exist.
Downloads are SHA256-checked. Stop supervisors before replacing a binary.

## Initialize your first project

Use a trusted application repository with a pushed `origin/main`, issues enabled,
and GitHub write access. The main checkout is never the implementation workspace.

```sh
cd your-application
aih init --provider codex
# Alternatively: aih init --provider claude-code
```

Review `.aih/project.yaml`, especially its detected verification commands, and
add project-specific architecture/invariants to `AGENTS.md`. AIH does not commit
your existing working changes. Commit and push the generated configuration:

```sh
git add .aih AGENTS.md
git commit -m "Enable AIH"
git push origin main
aih doctor
aih run "Implement the next small feature with tests"
aih watch
```

Verification must contain at least one applicable check. Install application
dependencies in the execution environment, or configure a trusted setup/check
command that does so. Checks are argument arrays, not implicit shell strings.
See [configuration and development](docs/DEVELOPMENT.md).

One provider is selected per project. Change `provider` on canonical main to switch.
Optional `provider_models` maps `normal`, `strong`, and `strongest` to model IDs;
empty mappings use the provider's own defaults. AIH does not invent model names.
`aih doctor` checks required CLI flags; older provider releases may need updating.

## Daily workflow

```sh
aih run --file requirement.md
aih status                 # --json for portable state
aih blockers
aih answer TASK_ID "Use the existing API contract"
aih logs
aih handoff                # stop workers, checkpoint, mirror issues, release lease
aih resume                 # background; --foreground for terminal operation
aih roles
aih roles explain security
aih rules --role reviewer --task TASK_ID
aih rules doctor
```

`run`, `answer`, and role assignment first queue a local durable request. Status
distinguishes these from remotely accepted work and shows rejected requests.
Do not delete local state while a request is only queued.

## Another laptop or recovery

```sh
git clone YOUR_GITHUB_REPOSITORY
cd YOUR_PROJECT
aih attach
aih status
aih resume
```

`attach` reconstructs and observes; `resume` executes. A live controller lease is
never overridden, including by `takeover`. After a crashed controller's lease
expires (plus a five-second grace), `aih takeover` starts a replacement.
Recovery preserves acknowledged remote checkpoints, not unpushed edits on a lost
disk. Keep machine clocks synchronized. See [recovery](docs/RECOVERY.md).

## Durability and GitHub Free

| Artifact | Owns |
| --- | --- |
| GitHub issues and PRs | Human-visible task relationships, blockers, evidence |
| Task branches | Pushed source checkpoints |
| `aih-state` | Versioned logical snapshot, ownership, decisions, retries |
| Per-project SQLite | Local commands, cached snapshot, events, runtime |
| Worktrees / sessions | Disposable execution files and local diagnostics |

The separate `aih-state` branch never merges into code. Final integration atomically
advances main, the task branch, and state with explicit expected revisions. This
approved design strengthens the specification's GitHub merge-endpoint approach;
see [decisions](docs/DECISIONS.md). PRs remain the evidence surface.
No GitHub Actions, paid protections, or hosted merge queue is required.

## Custom specialist roles

Commit a small role under `.aih/roles/` on main:

```yaml
name: api-compatibility
extends: reviewer
stage: review
capability: strong
permissions: [read]
output_schema: worker-v1
triggers:
  paths: ["api/**"]
focus: [backward compatibility, observable error semantics]
blocking:
  severities: [critical, high]
```

Manual assignment: `aih roles assign TASK_ID api-compatibility` for an idle task.
See [roles and context](docs/ROLES.md).

## Scope and safety

Use trusted repositories and a dedicated development environment without production
credentials. Configured checks execute local code. Provider permissions and secret
scanning are defense in depth, **not a hostile-code sandbox or secret-proof system**.
AIH never intentionally performs production or billing actions.

V1 has a textual dashboard, bounded workers, advisory custom roles using one strict
result envelope, and checksummed staged updates. It does not implement a web UI,
autonomous production rollback, GitHub-comment command parsing, multi-provider
scheduling, or in-place updating of running supervisors. Repo-specific visual
verification requires suitable checks/evidence; a model claim is not a screenshot.

Validation and remaining platform/provider limitations are recorded in
[VALIDATION.md](docs/VALIDATION.md). Start with a small non-production project.

## Details

[Architecture](docs/ARCHITECTURE.md) · [State](docs/STATE.md) ·
[Roles](docs/ROLES.md) · [Engineering rules](docs/ENGINEERING_RULES.md) ·
[Recovery](docs/RECOVERY.md) · [GitHub Free](docs/GITHUB_FREE.md) ·
[Development](docs/DEVELOPMENT.md) · [Releases](docs/RELEASES.md)
