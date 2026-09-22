# V1 engineering decisions

The central AIH repository is public. Target application repositories can remain
private; AIH supports GitHub Free and does not change their visibility.

## Accepted: atomic integration (2026-09-22)

AIH uses atomic Git reference publication rather than GitHub's merge endpoint.
A locally verified merge commit and a
separate orchestration-state commit are published together, conditional on the
expected main, task, and state revisions. `main` only advances; its history is
never rewritten. Task checkpoints use the same state fencing. GitHub PRs remain
the review/evidence surface and are reconciled after ancestry records the merge.
An unsupported atomic transport fails closed. Existing repository access rules
are not bypassed. This closes the race between a base/lease check and publication.

## Implementation boundaries

Go with a pure-Go SQLite driver; no services besides GitHub and provider CLIs.
The supervisor is the sole local writer of logical state. Local commands are
durable SQLite requests consumed by it. Commands report queued versus remotely
accepted state distinctly. Remote JSON snapshots include operation intents and
stable IDs; GitHub operations reconcile by those IDs after ambiguous failures.
Runtime PIDs, paths, logs, and provider sessions never enter the remote snapshot.
Repository instructions and policies are compiled from the fetched canonical
base revision. Source worktrees remain task-specific.

Recovery guarantees cover remotely acknowledged state and pushed checkpoints.
Unpushed edits on a lost machine cannot be recovered. Workers run in bounded
invocations so the supervisor can checkpoint without racing a writing process.
AIH is for trusted repositories on a dedicated development machine/VM; provider permissions
are defense in depth, not an OS isolation boundary against hostile code.

## Server deployment

Ubuntu deployment wraps the existing foreground supervisor in a non-root systemd
user service. Linger supplies boot/logout independence; normal lease acquisition
and recovery remain authoritative. SQLite and Git state stay on persistent local
disk and GitHub respectively. A container stack or external queue would add
dependencies without solving a V1 requirement, so neither is part of deployment.
