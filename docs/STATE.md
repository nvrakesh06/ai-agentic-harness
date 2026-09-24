# State and durability

Machine data defaults to `~/.aih` (override with `AIH_HOME` or `--home`):

```text
machine.yaml
projects/<stable-project-id>/
  registration.json
  state.db (+ WAL/SHM)
  supervisor.lock
  control.git/
  worktrees/<task-id>/
  scratch/<task-id>/
  sessions/<run-id>/
  logs/supervisor.log
  analysis/ integration/ post-verify/
```

With the Linux user service, supervisor stdout/stderr goes to the systemd journal;
`logs/supervisor.log` is only used by detached CLI starts. Events stay in SQLite
and worker diagnostics stay in sessions. See the [VM retention/backup runbook](VM_SETUP.md#storage-logs-and-backups).

Each task also has a local-only `scratch/<task-id>/` directory outside its source
worktree. AIH supplies it to workers as `AIH_SCRATCH`, `TMP`, `TEMP`, `TMPDIR`,
and npm's cache location. Use it for downloaded tooling, caches, temporary files,
and generated diagnostics; it survives checkpoint/resume and attach on the same
machine, is never a checkpoint candidate, and is removed after the task reaches
`DONE`.

The application contains only `.aih/project.yaml`, `policies.yaml`, `harness.lock`,
optional roles/platform files, and project-specific `AGENTS.md` instructions.
Never place AIH_HOME inside an application checkout or synchronize it between
machines. Install normal Git/provider credentials independently on each machine.

## Remote snapshot

`aih-state:snapshot.json` stores `state_schema`, `created_by_version`, project
identity, revision, controller lease, objectives, tasks, runs, accepted command IDs,
the ordered authorized objective backlog, capacity policy/status, improvement
candidates and the integration hold. Capacity state records active/target/maximum
writer utilization, queued preflight count, reader utilization, backlog cursor, grace boundary, latest
backfill selection, a machine-readable suppression reason and the bounded tail of
underutilization/selection/suppression transitions. It also records queued and
running native checks with their task, class, check
name, queue time and start time. Recovery discards interrupted check ownership
and follows the ordinary verification retry route. Task records contain dependencies,
conflict domains, issues/PRs, branch/base/head/merge revisions, retry counters,
findings, human decisions, portable preflight role progress, blocker/resume state, verification retry guards and exact
verification evidence. Passed checks record their identity, executable, exit result
and bounded redacted stdout alongside exact base/head/config/rules. A guard records
only portable environment/command classes,
the source revision, a normalized fingerprint and attempt count; it contains no
machine paths or provider conversation state.
No SQLite files or provider sessions are pushed.

Accepted cross-task guidance uses a bounded, versioned record in the existing
task decisions list. It contains the command ID, source task ID, exact source
checkpoint SHA, and correction text. This avoids a new remote schema while the
capacity-state migration is being deployed. A locally queued command is not
portable until the supervisor accepts and publishes it. Guidance enters the next
implementer prompt; if accepted mid-run, the completed worker's edits are
checkpointed and the task returns to READY for one corrected pass.

Git state commits form an independent append-only ancestry. Source/state changes
are fenced together; state-only changes use the same compare-and-swap mechanism.
SQLite is updated after remote acknowledgement. A crash in that gap is recovered
from the remote snapshot, not by replaying a stale cached state.

Local SQLite also stores a frequent supervisor heartbeat for `aih status`. It is
not portable and is never consulted for fencing. The durable snapshot heartbeat
and expiry are refreshed by meaningful saves or at lease half-life, whichever
comes first. Therefore remote lease information may be older than the local pulse
by up to half of `lease_seconds`, while the durable expiry remains authoritative.
At half-life AIH creates a lease-only `aih-state` commit using the existing snapshot
schema, so compatible older runtimes still observe the renewed fence. Lease-only
commits do not advance the durable state revision; the next meaningful save does.

SQLite uses WAL, FULL synchronization, and a busy timeout. Its snapshot is a cache;
commands are a local queue; events and runtime values are local observability.
A command marked queued is **not yet portable**. After remote acceptance its stable
ID is in the snapshot, preventing duplicate application after a local crash.

## Task states

`PLANNED -> READY -> RUNNING -> IMPLEMENTED -> SYNC_REQUIRED -> VERIFYING -> REVIEW
-> MERGE_READY -> MERGE_TRAIN -> POST_VERIFY -> DONE`

`READY` and `FIX` tasks first record pre-implementation reader guidance in a
portable preflight record. Completed roles resume after takeover; a change to the
canonical base, task contract/scope, accepted cross-task corrections, configuration,
or role rules invalidates the record. A changed task head alone can reuse fully
completed guidance for a bounded `FIX` retry or an implementer's durable
`in_progress` checkpoint continuation, with its durable scope fingerprint and
reuse reason recorded. A human unblock keeps that guidance only when the answer
identifies the exact checkpoint and introduces no new decision or specialist
direction.
Exact-head native checks and final independent reviews are never reused.
Shutdown and takeover clear interrupted reader ownership while preserving completed
guidance eligibility and its reuse counter across verification recovery.
The scheduler admits only prepared tasks to `RUNNING` after rechecking dependencies
and writer conflict domains. `RUNNING` therefore counts an actual reserved writer.
Failed checks/reviews take a bounded `FIX -> RUNNING` route. `BLOCKED_HUMAN`
records a question, reason, impact and resume state. Only that task and dependants
wait. Recovery maps interrupted writers to READY and interrupted verification to
SYNC_REQUIRED; POST_VERIFY resumes its exact recorded integration commit.
Questionless implementer blockers transition through IMPLEMENTED to supervisor-native
verification. Missing native capabilities and repeated failures at the same source
revision/environment block with resume state SYNC_REQUIRED, so a human can repair the
environment and recheck without starting another implementer.

## Schema compatibility and migrations

V1 uses remote schema 3, role schema 1, rules version 1, and local schema 1.
Unknown newer schemas fail closed before writes. Legacy schema 0 snapshots gain
version metadata and missing maps; schema 1 snapshots deterministically reconstruct
the authorized objective backlog from existing objectives. Both then undergo
validation. The first subsequent
state commit keeps the original remote commit as its parent, preserving the
pre-migration backup in Git history. No automatic major-version migration exists.

Schema 2 snapshots migrate to schema 3 with empty verification ownership and
the configured resource limits. Preflight progress is absent until the new
supervisor records it. An interrupted controller's task states still follow the
normal recovery route before checks start again.

Publishing schema 3 is a one-way deployment boundary: older runtimes reject
the newer snapshot. Upgrade every machine that may attach, resume, or take over
before allowing a schema-3 supervisor to acquire and publish state. Do not hand
control back to a schema-2 installation after that first schema-3 save.

Remote task identities and branches are constrained before use as filesystem or
Git targets. Schema changes require tests for old fixtures and new-runtime refusal.
Snapshots and normal state history are retained in V1; compaction is future work.

## Practical guarantees

Acknowledged remote state and pushed checkpoints survive deletion of all local
project data. Unpushed changes and queued-only commands on a destroyed disk do not.
An offline handoff reports failure and leaves local work available; it does not
pretend the remote is current. GitHub's presentation layer can temporarily lag the
authoritative snapshot after an API outage.
