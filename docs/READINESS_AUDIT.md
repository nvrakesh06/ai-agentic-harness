# Portability and publication audit

Scope: all tracked source, tests, scripts and documentation; ignored build
artifacts; metadata and all reachable history (one initial commit at audit time).
The worktree was clean before this phase.

## Preserved architecture

Single Go CLI/supervisor with Cobra, YAML and pure-Go SQLite. Non-interactive
Codex/Claude adapters, supervisor-owned Git/GitHub operations, task worktrees,
bounded workers/reviews/checks, local command queue/cache, portable `aih-state`,
and fenced atomic publication all remain. No state schema changed. Windows
detached operation remains; there is no frontend, listener, external queue,
container deployment dependency, Actions workflow or paid GitHub requirement.

## Gaps addressed

| Area | Finding / action |
| --- | --- |
| First run | Separate harness installation from application enabling; bootstrap on Unix/Windows |
| Configuration | Explicit literal environment files; agent executable and fork release overrides |
| Linux | amd64/arm64 release targets and installer support; native Ubuntu test environment |
| Service | Non-root systemd user unit around foreground startup; boot/logout via linger |
| Recovery | Preserve leases/state; real process-death and persistent-queue restart test |
| Git race | Stop implicit push tracking updates; serialize explicit fetches across local CLI processes |
| Storage | Private new SQLite files; resolve symlinks when excluding source-local state |
| Doctor | Platform/tools/auth/storage/config/check binaries/remote state diagnostics |
| Operations | Headless login, lifecycle, stale leases, logs, backups, disk growth, updates |
| OSS | MIT, contributing/security/conduct, ignore hygiene and practical quick start |

Systemd retries failed startup instead of weakening the lease. No state migration
or deviation from the approved Git integration strategy was necessary.

## Security findings

No obvious committed credentials, private keys, personal workspace roots, cloud
endpoints, databases or agent transcripts were found. Tests assemble synthetic
token patterns at runtime. `aih@localhost` is the neutral automation identity.
The Go module, upstream downloads and links intentionally name this public project,
not a required login identity. Existing history uses a GitHub no-reply author
address. No history rewrite was performed.

The repository was already public. Pattern inspection is not an exhaustive audit
and does not inspect private provider/account files. Private vulnerability reporting
is enabled upstream; forks need their own route. Rotate any discovered credential
independently of source cleanup.

Native checks remain unrestricted as the service user. Providers may access local
files; output may contain sensitive data; GitHub write access is powerful. No
hostile-code isolation, automatic credential rotation or multi-tenancy is claimed.

## Deliberately not added

No distributed queue, container stack, secret manager, cloud provisioning,
telemetry service, web dashboard, auto-pruning of recovery data or global Git
configuration automation. These add permissions/dependencies or recovery hazards
without being necessary for one V1 supervisor. Disk retention is an operator task.

See [VALIDATION.md](VALIDATION.md) for actual results. Unit generation and cross-
compilation do not prove a real VM reboot or paid-provider end-to-end workflow.

## Release-candidate review (2026-09-22)

Reviewed all 77 candidate files and the complete local diff against the original
V1 goals and initial implementation. No further architecture, state schema,
provider protocol, Git publication, or orchestration change was needed. Final
fixes normalize CRLF/LF in policy-drift diagnostics (with a regression test) and
exclude copied GitHub CLI credentials, Claude machine settings and AIH update
caches from ordinary Git additions.

README, Go requirements, bootstrap/installers and VM/recovery runbooks agree:
Go 1.21+ bootstraps the selected Go 1.27.1 toolchain, the module minimum is 1.26,
and installed binaries need no Go runtime. The systemd unit wraps the existing
foreground supervisor; Windows retains detached local operation. Neither requires
Actions, paid protections, Docker, a hosted queue, or another infrastructure service.

LICENSE, contribution/conduct/security policies and private reporting were reviewed.
No issue/PR templates exist; practical report and review requirements are in
CONTRIBUTING.md. No template framework or workflow automation was added. Broad
credential-pattern inspection found only safe example placeholders. Ignore probes
covered 19 disposable/sensitive paths and preserved seven required config/source
paths, including `.env.example` and portable `.aih` policy. No ignored or runtime
files are staged. A forced Git addition can still bypass ignore rules; inspect
the staged diff before every publication.
