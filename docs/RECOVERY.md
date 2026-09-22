# Recovery runbook

## Intentional machine switch

1. On A, run `aih handoff`. Scheduling stops; workers terminate; safe work is
   checkpointed after writers exit; issues are mirrored; the lease is released.
2. Do not discard A's workspace if handoff reports unpersisted work.
3. Clone the configured application on B. Install/authenticate Git, gh and its
   selected provider independently. Run `aih attach`, inspect `aih status`, then
   `aih resume`.

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

When the old disk is gone, recovery starts at the last remotely acknowledged
checkpoint. Unpushed edits cannot be reconstructed. On the same machine, existing
worktrees are retained rather than reset, so uncommitted work can be checkpointed
after inspection. Interrupted review/test processes are disposable and rerun.

If local state is corrupt, first stop the supervisor and preserve its entire
project directory somewhere safe. Recreate through attach rather than copying a
SQLite database from another machine. Never delete the authoritative `aih-state`
branch or manually merge it into main.

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

`aih answer TASK_ID "decision"` (or a task's issue number) records the answer remotely
before rescheduling. Planning blockers use the objective ID shown by status.
Answers should contain decisions, never credentials. Supply secrets through the
local application/provider mechanism, not the command payload or GitHub comments.

`aih demo` exercises three completed tasks, a persistent blocker, serialized
integration, local directory deletion and reconstruction without live services.
