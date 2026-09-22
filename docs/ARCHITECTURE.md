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

Writers are limited to three (configurable downward), each with a branch/worktree.
Dependencies must be DONE. Active conflict domains exclude overlapping writers.
Advisory roles and native checks share a separate bounded reader semaphore. A task
remains reserved across its merge workflow, including fresh-main re-review.
The scheduler keeps running while only blocked or dependency-waiting tasks remain.

The machine lease is not distributed consensus. Correct clocks and atomic Git push
are required. An expired controller stops publication even before takeover. GitHub
issue/PR body updates are not part of Git's transaction: they are idempotently
reconciled and cannot serve as a code-publication fence.

## Worker lifecycle

Each provider invocation is bounded (default 15 minutes). Structured output reports
completed, in_progress, blocked, or failed. Code is checkpointed after the writer
exits, never while it is changing files. Retry/rotation budgets prevent endless
loops; repeated failures get one Advisor recovery approach before escalation.
No provider conversation is needed to restart a worker.

Codex uses native read-only/workspace-write permission modes and structured result
files; canonical instructions are supplied explicitly. Claude uses safe mode and
an explicit read-only or edit tool list, without model shell execution. Native
checks are supervisor-owned. Authentication remains in the provider's normal local
installation. Providers do not receive GitHub token environment variables.

Windows workers start suspended, join a kill-on-close Job Object, then resume.
Unix workers use an owned process group and pipe-lifeline guardian so supervisor
death also kills descendants on macOS. Deliberately daemonizing hostile code is
outside this trusted-repository boundary. Background supervisors are detached from
their launching terminal.

## Fresh-main and integration protocol

1. Fetch main; pin one immutable commit for all configuration/context reads.
2. Rebase the task onto it; on conflicts prepare a supervisor-owned merge for the
   writer to resolve. The durable conflict base allows reproduction elsewhere.
3. Checkpoint; run native checks and independent required roles. Record exact base,
   head, configuration hash, rule hash and evidence.
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
