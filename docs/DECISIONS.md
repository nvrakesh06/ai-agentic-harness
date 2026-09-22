# V1 engineering decisions

The user requested public visibility for the central AIH repository, overriding
the original specification's private-repository default. Target application
repositories can remain private; AIH still supports GitHub Free.

## Approved: atomic integration (2026-09-22)

The user approved replacing specification section 43's GitHub merge endpoint
with atomic Git reference publication. A locally verified merge commit and a
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
AIH is for trusted repositories on a developer machine; provider permissions
are defense in depth, not an OS isolation boundary against hostile code.
