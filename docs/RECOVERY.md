# Recovery runbook

## Intentional machine switch

1. On A, run `aih handoff`. Scheduling stops; workers terminate; safe work is
   checkpointed after writers exit; issues are mirrored; the lease is released.
2. Do not discard A's workspace if handoff reports unpersisted work.
3. Clone the configured application on B. Install/authenticate Git, gh and its
   selected provider independently. Run `aih attach`, inspect `aih status`, then
   `aih resume`.

Before a deployment that changes the remote state schema, upgrade both the current
and replacement machines. Once the newer supervisor publishes state, an older
runtime fails closed and cannot be used for attach, resume, or takeover.

`attach` checks provider/runtime compatibility, fetches canonical configuration,
loads remote logical state, queries issues/PRs, and recreates checkpoint worktrees.
It is observational; it does not steal ownership or run agents until resume.
`sync` performs the same reconstruction and requires a stopped local supervisor.

For a systemd-managed VM, disable the old service before switching machines:
`systemctl --user disable --now aih-NAME.service`. An enabled service can try to
reacquire ownership after reboot. Check logs for successful checkpoint/release;
preserve local data if shutdown reports unpersisted work.

The user service restarts failed foreground processes every 15 seconds. After an
unclean stop it retries normal lease acquisition; it does not steal even its own
machine's live lease. See [VM setup](VM_SETUP.md).

## Crash, terminal closure or lost disk

Closing the launching terminal does not stop a detached supervisor. Use `aih stop`
or `aih handoff` to stop it. Foreground operation responds to interruption.
If the supervisor dies, owned workers are terminated and logical state remains on
GitHub/Git. Wait for the remote lease to expire plus five seconds, then use
`aih takeover` (or resume). No command silently overrides a live lease.
If `aih resume` reports that the supervisor is still initializing, its local lock
is held but lease acquisition has not yet been acknowledged. Use `aih status` or
`aih watch` to observe the result instead of starting another controller. A
reported startup error includes the supervisor log path for diagnosis.
The local status heartbeat may stop before the durable lease expires; it does not
shorten this wait. During normal operation, meaningful state saves refresh the lease
immediately and otherwise the supervisor renews it at half-life, so a live owner
remains fenced without publishing on every local heartbeat.

When the old disk is gone, recovery starts at the last remotely acknowledged
checkpoint. Unpushed edits cannot be reconstructed. On the same machine, existing
worktrees are retained rather than reset, so uncommitted work can be checkpointed
after inspection. Interrupted review/test processes are disposable and rerun.

An implementation worker approaching its deadline receives one bounded checkpoint
pass before hard termination. If that pass cannot return structured output, AIH
creates a sanitized local session `handoff.json`, then atomically publishes safe
worktree edits and the synthetic `in_progress` summary/check/risk/decision fields.
The local JSON is diagnostic-only; replacement machines recover from the matching
remote task branch and snapshot. Command evidence is restricted to fixed check
families without arguments. Non-timeout checkpoint errors take the normal retry and
Advisor path. The next worker receives the portable handoff in its assigned task.
Inspect `aih status`, `aih watch`, or `aih logs` for `worker_checkpoint_requested`
and `worker_hard_timeout` lifecycle events. A synthetic handoff is recovery evidence,
not proof that the implementation or its tests are complete.

If local state is corrupt, first stop the supervisor and preserve its entire
project directory somewhere safe. Recreate through attach rather than copying a
SQLite database from another machine. Never delete the authoritative `aih-state`
branch or manually merge it into main.

## Explicit legacy scope reauthorization

Schema 9 intentionally blocks a started legacy task when it has no provable
immutable assignment. Do not edit SQLite, infer an old scope from `areas` or a
diff, or weaken that gate. An operator may instead authorize a **new** bounded
contract with `aih scope recover --file recovery.json`. This command does not
start providers or schedule work.

First stop the supervisor, fetch state, and inspect the task checkpoint and
retained worktree. Create a schema-1 JSON manifest from that exact remote state:

```json
{
  "schema": 1,
  "command_id": "scope-reauth-20260926",
  "expected_state_ref": "<aih-state commit>",
  "policy_hash": "<current canonical policy hash>",
  "tasks": [{
    "id": "legacy-task",
    "base_sha": "<durable task base>",
    "head_sha": "<durable task head>",
    "contract_hash": "<existing acceptance/scope contract hash>",
    "areas": ["internal/example"],
    "additional_dependencies": ["predecessor-task"],
    "reason": "Operator explicitly authorizes this new bounded ownership contract."
  }]
}
```

Run `aih scope recover --file recovery.json --preview` first; it performs no
lease acquisition or publication. If its proof is accepted, run
`aih scope recover --file recovery.json`, then inspect `aih status` and use
`aih resume` only when the ordinary scheduler should continue. Recovery checks
the exact state/policy, checkpoint ref,
base-to-head and dirty-worktree paths, canonicalizes the declared areas at the
current canonical base, preserves existing dependencies, rejects conflicts with
unfinished owners unless a predecessor dependency gates them, and rejects cycles.
It refuses a live owner, active work, merged work, or an already known immutable
assignment. A successful command records the authorization in the task's typed
decision history, invalidates preflight/review/visual approval evidence, and
keeps findings, budgets, summaries, checkpoints, and retry guards. Blocked tasks
remain blocked; other unmerged recovered tasks return to `READY` and must complete
fresh preflight and verification. Reuse the same command ID only for an
idempotent retry.

Historical planned-reference restoration is intentionally future work. This
phase records explicit operator authorization only.

## Network or permission failures

Git publications compare explicit expected ref revisions. A mismatch or uncertain
acknowledgement never triggers an unconditional force push. If remote state confirms
the exact new transaction, it is acknowledged; otherwise AIH stops/reconciles and
reports retained work. Restore access, inspect logs, and resume after any lease
expiry. Existing GitHub protections may reject direct integration; AIH will not
disable or bypass them.

GitHub issue/PR creation uses stable markers to recover ambiguous responses. Body
updates can lag during outages. Handoff retries issue mirrors. The machine-readable
snapshot and Git branches are the recovery authority, not a recent issue comment.

## Merge conflicts and failed verification

An automatic rebase conflict is aborted, then main is prepared as a pending merge
for the writer to resolve. The conflict base is portable, allowing reconstruction
on another machine. Unresolved conflict markers cannot be checkpointed as complete
work. Retry budgets and Advisor escalation prevent endless attempts.

A pre-integration failure never changes main. A post-integration failure has already
changed main: AIH sets a global integration hold, continues unrelated implementation,
and asks for a repair/revert decision. Do not assume an automatic rollback happened.
Fix the failing check/environment or provide a human-reviewed repair/revert through
the repository's normal process, then answer the blocker to recheck. Checks run on
the recorded integration commit during normal operation. After a human answers a
post-merge blocker, they check the latest descendant of that commit, allowing a
human-directed repair or revert to recover the train. Recovery on a newer commit
also requires independent QA. If main is healthy but task acceptance is no longer
satisfied (for example after a revert), the train hold clears and the task remains
blocked. Submit a separate repair objective, then answer the original task after
the repair merges. This keeps an already-merged PR's history intact rather than
trying to reuse it as an open PR. Rewritten main history is rejected.

## Human decisions

## Bounded task replacement

`aih task replan --file repair.json` is a one-shot supervisor operation for an
idle, unmerged group of tasks in one objective. The versioned manifest pins the
exact current `aih-state` ref, canonical policy, source checkpoints, and one
replacement contract. It can include a task with an empty checkpoint only when
the remote snapshot proves that task was never started; such a task has no source
patch and its declared branch must be absent remotely.
It creates a new successor branch and records each original as `SUPERSEDED`; an
original never becomes `DONE`, and its dependants wait for the successor's normal
verification and integration. Phase one deliberately rejects cross-objective
groups. A resolved operator candidate head is permitted only when it descends from
canonical main, contains every declared source checkpoint, and validates entirely
within the replacement's immutable areas. It never waives native checks or review.

`aih answer TASK_ID "decision"` (or a task's issue number) records the answer remotely
before rescheduling. Planning blockers use the objective ID shown by status.
Answers should contain decisions, never credentials. Supply secrets through the
local application/provider mechanism, not the command payload or GitHub comments.

`aih demo` exercises three completed tasks, a persistent blocker, serialized
integration, local directory deletion and reconstruction without live services.
