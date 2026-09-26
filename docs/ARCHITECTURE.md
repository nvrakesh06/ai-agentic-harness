# Architecture

AIH is a single Go binary. Cobra implements the CLI; a pure-Go SQLite driver
provides the local cache/command queue. There are no database or queue services.

`model` owns portable records and scheduling eligibility. `engine` coordinates
planning, work, review, synchronization, integration and recovery. `gitx` and
`github` own deterministic external operations. `roles` compiles scoped canonical
context; `provider` encapsulates the Codex/Claude protocols. `platform` owns
processes and locks. `demo` runs the same engine with fake intelligence/GitHub.

## Ownership and scheduling

One local file lock excludes duplicate supervisors on a machine. A remote lease
contains machine ID, invocation owner, monotonically increasing epoch, heartbeat
and expiry. Acquiring and renewing it use explicit expected `aih-state` revisions.
Every checkpoint and integration publishes state in the same atomic ref transaction
as source, so a superseded controller cannot silently publish source afterward.

The supervisor records a machine-local liveness pulse at least every 30 seconds.
That pulse is observability only and grants no write authority. Durable remote lease
renewal occurs at half of the configured lease lifetime; every meaningful state
publication also refreshes the lease and moves that boundary. Duplicate mutations
and intervening local pulses are semantic no-ops, so they do not create state commits.
Lease-only renewals keep the current snapshot schema for compatibility but do not
increment the state revision. This keeps takeover fencing on the remote
compare-and-swap path while bounding steady-state lease history to two publications
per lease duration.

Writers are limited to three (configurable downward), each with a branch/worktree.
Pre-implementation reader guidance enters a bounded preflight queue before writer
admission. A fully completed preflight may bypass that reader queue for a bounded
FIX retry only when its durable task-scope, base, policy, and role-rule inputs still
match; exact-head verification and final reviews still run after every code change.
At most `max_readers + 1` guidance tasks and `max_writers` tasks without
guidance prepare concurrently, so reader saturation cannot hide ready coding work.
Dependencies must be DONE. Active conflict domains exclude overlapping writers.
Advisory roles use a bounded reader semaphore. Native checks use separate heavy
and light resource limits; heavy checks also hold machine-local lock slots under
the shared AIH home across project controllers. A task
remains reserved across its merge workflow, including fresh-main re-review.
The scheduler keeps running while only blocked or dependency-waiting tasks remain.
A canonical target controls best-effort useful writer utilization below that hard
maximum. The first queued objective starts immediately. When safe tasks later
cannot meet the target beyond the configured grace period, the controller advances
an ordered backlog containing only accepted `aih run` objectives. The portable
snapshot records the policy, backlog cursor, latest dispatch, exact suppression
reason and a bounded transition tail. Identical idle decisions do not create a
state commit on every scheduler tick.

The machine lease is not distributed consensus. Correct clocks and atomic Git push
are required. An expired controller stops publication even before takeover. The
machine-local pulse never extends the durable expiry. GitHub
issue/PR body updates are not part of Git's transaction: they are idempotently
reconciled and cannot serve as a code-publication fence.

## Worker lifecycle

Each implementation budget is bounded (default 15 minutes). The normal worker gets
80 percent of that budget. If it reaches this soft deadline with recent provider
output or worktree changes, AIH terminates that process before starting one fresh,
narrowly prompted checkpoint pass in the remaining budget. The checkpoint pass must
stop expanding scope, preserve safe edits, run only the smallest relevant check and
return the normal structured result. There is only one pass and the original hard
deadline never moves.

If the checkpoint pass still reaches the hard deadline, the supervisor records a
sanitized `handoff.json` in the local session and synthesizes an `in_progress`
handoff from changed files, Git status and allowlisted command-family evidence;
command arguments never enter portable state. One fenced ref transaction publishes
the safe source checkpoint together with its summary, checks, risks, decision and
rotation state. `handoff.json` is local diagnostics only. Non-deadline checkpoint
failures use the ordinary failure/Advisor budget. Idle workers without
recent output or edits stop at the soft boundary without grace. Deadline phases are
recorded as lifecycle events and shown by `status`/`watch`.

Structured output reports completed, in_progress, blocked, or failed. Code is
checkpointed only after the writer process exits, never while it is changing files.
Retry/rotation budgets prevent endless loops; repeated failures get one Advisor
recovery approach before escalation. No provider conversation is needed to restart
a worker.

For a correction discovered across parallel tasks, `aih guide <target-task-id>
--from <source-task-id> --file <text-file>` queues at most 1600 UTF-8 bytes.
The supervisor accepts it only for distinct tasks in the same objective, with a
durable source code checkpoint and a target in READY, RUNNING or FIX. The accepted
command and source head are recorded in portable task state; secret-like text is
rejected before publication. The next implementer
invocation receives it explicitly. A correction accepted while a worker is running
does not interrupt that provider call; the supervisor checkpoints its source edits
and schedules one more bounded pass before verification. It does not deliver a
live message inside an already-running provider call.

An implementer blocker with a human question becomes `BLOCKED_HUMAN`. A questionless
implementer blocker means the source change is complete but the worker environment
could not finish verification; the supervisor checkpoints it and proceeds directly
to canonical native checks. Portable task state records the native environment,
source revision and normalized failure fingerprint. A missing native executable, or
a repeated native failure at the same revision and environment after a no-change fix,
becomes a durable verification blocker instead of launching another equivalent writer.
Code failures still use the normal bounded `FIX` route.

A supervisor-native timeout, and a Windows npm diagnostic that explicitly reports
`code EPERM`, `syscall unlink`, and a `node_modules` installation path, each receive
one exact-head retry without consuming an implementer or Advisor budget. A second
transient failure for the same native environment, head, and complete native-check plan becomes a
verification-only `BLOCKED_HUMAN` checkpoint with a sanitized diagnostic. The retry
does not synchronize or rebase its already-checked head; changing that head, native
environment, or canonical check plan starts a new bounded allowance. Cancellation and
ordinary compiler or assertion failures never use this lane.

Codex uses native read-only/workspace-write permission modes and structured result
files; canonical instructions are supplied explicitly. Claude uses safe mode and
an explicit read-only or edit tool list, without model shell execution. Native
checks are supervisor-owned. Authentication remains in the provider's normal local
installation. Providers do not receive GitHub token environment variables.

An authoritative provider authentication failure or an `invalid_json_schema`
rejection creates a schema-12 portable admission hold before any task-local retry
route. The hold suppresses planning, preflight, writing, review, and Advisor calls
for its active provider scope without consuming their budgets; native verification
and integration remain schedulable. Authentication is keyed to the provider. A
schema rejection is keyed to the provider and exact output-schema digest, so a
policy, rules, or model change cannot repeatedly reopen it. Origin policy, rules,
and model remain audit provenance only. The bounded map keeps six exact holds plus
one non-evicting saturation hold per supported provider. A public retry is a queued,
supervisor-owned read-only protocol probe. It never dispatches a task worker and
removes only its selected active hold together with the command's portable applied
receipt after a valid completed response.

Windows workers start suspended, join a kill-on-close Job Object, then resume.
Unix workers use an owned process group and pipe-lifeline guardian so supervisor
death also kills descendants on Linux and macOS. Deliberately daemonizing hostile code is
outside this trusted-repository boundary. Background supervisors are detached from
their launching terminal.

## Fresh-main and integration protocol

1. Fetch main; pin one immutable commit for all configuration/context reads.
2. Rebase the task onto it; on conflicts prepare a supervisor-owned merge for the
   writer to resolve. The durable conflict base allows reproduction elsewhere.
3. Checkpoint; run native checks and independent required roles. Record exact base,
   head, configuration hash, rule hash and evidence.
   Successful native evidence includes bounded/redacted stdout, check/executable
   identity and exit status. Reviewer, security, and designer roles run concurrently;
   built-in QA then receives their completed exact-head artifacts and remains an
   independent acceptance gate.
   A supervisor-evidence request triggers one native refresh and role-only retry;
   concrete blocking findings still enter FIX and true decisions still block human.
4. Reserve the merge train. If main, policy, rules or head changed, repeat checks
   and review. Verify the PR is still open, on main, with the expected task head.
5. Construct a standard two-parent integration commit with the verified tree.
   Verify that exact commit in a separate detached worktree.
6. Atomically publish `main: B -> M`, `task: H -> M`, and `aih-state: S -> S'` using
   explicit leases for all three. Main and state must advance in ancestry.
7. Run post-integration checks on M. Mark DONE only on success. A failure blocks
   further integration until a human-directed repair/revert and successful recheck.

The task ref intentionally changes during step 6. Git omits no-op ref updates,
which would otherwise silently omit the task-head comparison. A remote main race
causes re-synchronization, never a blind force push. Network acknowledgement loss
is reconciled using the unique state commit. Unsupported atomic pushes fail closed.

## Trust boundary

No AIH branch is a security enclave. Anyone with repository write permission can
edit it; protect access appropriately. Checks execute repository-controlled code
as the developer, and provider authorization can still read local files. Do not
run untrusted repositories or keep production credentials in the execution account.
Secret scanning is heuristic; review initial setup and unusual checkpoints.

## Server operation

`aih service install` renders a systemd **user** service around the unchanged
`start --foreground` path. Linger supplies boot/logout independence; restart on
failure retries normal lease acquisition, never overrides it. The supervisor
still owns all checkpoints and recovery. No queue, state schema, daemon protocol,
container runtime, or network listener was added. Windows keeps detached CLI
startup. See [VM setup](VM_SETUP.md).

Machine configuration is process environment plus an explicitly selected literal
environment file. Credentials remain outside portable project policy. `doctor`
checks tools/auth status, canonical config and local storage without calling a
model or executing application checks.
