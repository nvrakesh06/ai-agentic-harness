# Optional lightweight coordinator workflow

This is a documented, optional way to run small, well-bounded parallel work
without an AIH supervisor. A human or designated coordinator owns dispatch,
ordinary Git/GitHub publication, and integration. It is useful when the team
wants independent worktrees and agent sessions but does not need AIH's durable
task scheduler or reconstruction guarantees.

It is not an `aih` mode or command. Do not add a wrapper executable or invent
an `aih lightweight` invocation. Use native provider CLIs and ordinary Git/GitHub
operations.

## Boundary with supervised mode

| Concern | Supervised mode | Lightweight mode |
| --- | --- | --- |
| Controller and publication | The single AIH supervisor owns checkpoints, issues/PRs, checks, and fenced integration. | The named coordinator owns normal Git/GitHub operations. |
| Durable state | Local operational SQLite plus remote `aih-state`, leases, receipts, and recovery rules. | Read legacy state only to prepare a handoff; never write or emulate it. Keep ordinary Git/GitHub and coordinator records. |
| Recovery claim | A supported AIH reconstruction path at its recorded state boundary. | Recover from the task branch, commit, PR, coordinator handoff, and provider-supported session artifacts only. |
| Parallelism | AIH schedules bounded writers/readers and validates its configured policy. | The coordinator dispatches only genuinely independent work and serializes integration. |

Lightweight work must never write `aih-state`, AIH SQLite, AIH receipts, leases,
or run metadata. It does not receive equivalent recovery, fencing, or portable
state guarantees. A stale remote AIH state is a frozen historical record, not a
live lightweight ledger. Do not alter it with SQL, synthetic receipts, or state
commits. Moving work back to supervised mode is a separate supported adoption
action after lightweight work is stopped and reconciled; it is never a database
edit or a concurrent resume.

## Transition from supervised work

Choose one mode for a project at a time.

1. Stop or hand off the old supervisor cleanly (`aih stop` or `aih handoff` in
   the application checkout) and confirm that no supervisor or worker remains
   active. Do not begin lightweight work while a lease-bearing supervisor could
   publish.
2. Save a transition record in an ordinary, coordinator-owned location. For
   every task, record its task ID/title, owner, branch and worktree, exact head
   SHA, PR/issue if any, status, dependency, and blocker. Preserve the mapping
   even for completed or blocked tasks.
3. Preserve all legacy branches, retained worktrees, local AIH state, and the
   remote `aih-state` branch. Do not delete or reinterpret them as lightweight
   records.
4. Mark the coordinator record as **lightweight active** and do not call
   `aih resume` for the project. If a return is needed, first stop and reconcile
   lightweight tasks, then use the release's supported handoff/adoption process.

The reverse transition has the same exclusivity rule: do not restart a legacy
supervisor while any lightweight writer, review, or integration is active.

## Plan and dispatch only ready, independent tasks

Use two or three writers only when their changes are genuinely independent:
separate files or change domains, no unresolved ordering, and no shared
generated outputs, migrations, release files, or API contract edits. A task that
depends on another task's unpublished result is read-only on that dependency;
do not guess at its future shape. Re-slice or stage dependent work instead of
creating parallel conflict.

Before dispatch, complete a handoff such as
[the reusable template](templates/lightweight-task.md). It must state all of
the following:

- one accountable implementer and one dedicated worktree;
- exact base branch/SHA, branch name, owned paths, and explicitly read-only
  dependencies;
- outcome, exclusions, acceptance criteria, applicable repository commands,
  and expected evidence;
- known blocker/decision owner, risk level, and whether security, QA, UI, or
  animation evidence applies; and
- the review packet required at the resulting commit: exact SHA, diff range,
  commands/evidence, and a read-only reviewer assignment.

One worktree has one implementer. A reviewer uses a distinct read-only session
at the actual committed head; a reviewer is not a second writer in the task
worktree. Backfill a vacated writer slot as soon as another fully specified,
independent task is available. Report observed active writers, ready independent
backlog, and elapsed lifecycle times; do not promise a speedup or manufacture a
throughput figure from model activity.

### Example project model policy

The following is an example policy, not an AIH requirement or a statement about
model availability: use Terra for implementers, GPT-6 Sol for reviewers, and
Astra only for consequential architecture decisions. Do not use Luna in this
example. A project may choose different installed model identifiers and decision
thresholds, but should record its policy in each task handoff before dispatch.

Use the strong architecture lane sparingly: irreversible interfaces, significant
security boundaries, migrations, or cross-task design changes qualify; ordinary
implementation and review do not. An implementer should repair the assigned
change directly against the completed handoff rather than repeatedly asking a
model for broad preflight permission. Ask a focused question only when a real
blocker or contract ambiguity appears.

## Native sessions, logs, and worktrees

Keep the worktree and every log directory separate. The examples use placeholders
for model IDs and put JSONL and final-message files under an operating-system
temporary directory, outside the source tree. They are direct `codex exec`
commands, not a wrapper. Confirm the named options against the installed Codex
CLI before use.

POSIX example for an implementer:

```sh
repo=/absolute/path/to/application
task_worktree="$repo/.worktrees/task-123"
run_dir=/var/tmp/aih-lightweight/task-123

git -C "$repo" fetch origin main
git -C "$repo" worktree add -b coordinator/task-123 "$task_worktree" origin/main
mkdir -p "$run_dir"

printf '%s\n' 'Implement task-123 exactly as the handoff states. Own only the listed paths. Run the listed focused checks and report their commands and results.' \
  | codex exec -C "$task_worktree" --model '<IMPLEMENTER_MODEL>' \
      --sandbox workspace-write --json \
      --output-last-message "$run_dir/implementer-last-message.txt" - \
      > "$run_dir/implementer.jsonl"
```

PowerShell uses the same explicit working directory and keeps outputs outside
the worktree:

```powershell
$repo = 'C:\repos\application'
$taskWorktree = "$repo\.worktrees\task-123"
$runDir = 'C:\Users\you\AppData\Local\Temp\aih-lightweight\task-123'

git -C $repo fetch origin main
git -C $repo worktree add -b coordinator/task-123 $taskWorktree origin/main
New-Item -ItemType Directory -Force -Path $runDir | Out-Null

'Implement task-123 exactly as the handoff states. Own only the listed paths. Run the listed focused checks and report their commands and results.' |
  codex exec -C $taskWorktree --model '<IMPLEMENTER_MODEL>' `
    --sandbox workspace-write --json `
    --output-last-message (Join-Path $runDir 'implementer-last-message.txt') - `
    1> (Join-Path $runDir 'implementer.jsonl')
```

Record the provider's emitted session identifier and JSONL output in the
coordinator log. Record a wall-clock deadline before dispatch; the coordinator
monitors it and stops a stalled or expired session instead of leaving it running.
Use supported provider resume/interruption behavior where available, or terminate
only the identified, coordinator-owned process tree using the operating system.
Never kill unrelated sessions by process name. Do not edit provider session data,
reuse an unrelated transcript, or depend on an undocumented scheduler trick.
When a session ends or times out, record
the exact result and head in the handoff, then either dispatch a new bounded
session or mark a blocker.

After the assigned commit owner records the implementation, give the reviewer an exact immutable target and
a separate worktree. The sandbox setting remains explicit, while the prompt
forbids edits:

```sh
review_worktree="$repo/.worktrees/review-123"
task_head='<TASK_COMMIT_SHA>'

git -C "$repo" worktree add --detach "$review_worktree" "$task_head"
git -C "$repo" diff --stat origin/main..."$task_head"

printf '%s\n' "Read-only review of task-123 at $task_head. Review origin/main...$task_head and the supplied command evidence. Do not edit. Report introduced defects and consequential relevant inherited blockers with file/line, severity, and a concrete remedy." \
  | codex exec -C "$review_worktree" --model '<REVIEWER_MODEL>' \
      --sandbox read-only --json \
      --output-last-message "$run_dir/reviewer-last-message.txt" - \
      > "$run_dir/reviewer.jsonl"
```

Focused reviewer feedback while an implementer is editing may clarify an active
concern, but it is not an approval of uncommitted work. The final disposition is
tied to the committed SHA, diff range, and commands actually run.

## Evidence, review, and repair

Review only the task's introduced changes plus consequential inherited blockers
that matter to the changed behavior. Do not turn the review into a general
cleanup pass, but do not dismiss a real defect merely because its origin
predates the branch. Findings should identify the commit/diff, location, impact,
severity, and remedy. The implementer repairs concrete findings directly, then
re-runs the affected focused checks; repeated model preflight cycles are not a
required ceremony.

Apply QA, security, UI, animation, and architecture review when repository rules
or the actual risk require them. There is no blanket role checklist in lightweight
mode. A security-sensitive path or threat-model change needs security review; a
UI change needs the applicable UI evidence; an animation change needs rendered
frames, not a claim that a timeline was inspected. Visual review means inspecting
the actual screenshots for the changed states. A missing capture/frame is missing
evidence, not a pass.

Run all repository-required checks before merge. Run focused checks while
editing, and reserve one bounded heavy-validation lane for the final candidate
instead of starting duplicate expensive validations across writers. Capture each
command, working tree/commit, exit result, and relevant artifact location in the
handoff or PR. A review without the actual diff/commit/commands is incomplete.

For documentation-only changes in this repository, check the rendered/relative
links, command examples, and `git diff --check`, then inspect the diff. They do
not require binary acceptance or deployment validation. Any executable or
runtime change still requires the repository's full `go test ./...`, `go vet
./...`, and release cross-build gate.

## Publish and integrate serially

The coordinator publishes through ordinary Git/GitHub. For example, after review
and required checks pass:

```sh
git -C "$task_worktree" status --short
git -C "$task_worktree" push -u origin HEAD:coordinator/task-123
gh pr create --repo OWNER/REPO --base main --head coordinator/task-123 \
  --title 'task-123: concise outcome' --body-file /var/tmp/aih-lightweight/task-123/pr-body.md
```

Before each integration, refresh `main` and test the candidate's interaction
with that fresh base in a disposable integration worktree. The coordinator
integrates one accepted task at a time, then refreshes again before considering
the next task:

```sh
integration_worktree="$repo/.worktrees/integrate-task-123"
git -C "$repo" fetch origin main
git -C "$repo" worktree add --detach "$integration_worktree" origin/main
git -C "$integration_worktree" merge --no-commit --no-ff "$task_head"
# Run the task's required checks and affected-interaction checks here.
# Recheck that remote main still matches the tested base; rerun if it changed.
gh pr merge --repo OWNER/REPO <PR_NUMBER> --merge --match-head-commit "$task_head"
git -C "$repo" fetch origin main
```

Use the repository's required merge method and protections; do not bypass them.
If fresh-main validation exposes a conflict or interaction defect, stop that
integration, record the new head/blocker, and return it to its owner. After the
ordinary GitHub merge, recheck affected interactions on the merged `main` before
serially integrating another task.

Checkpoint the coordinator record only at meaningful lifecycle transitions:
dispatched, committed, review accepted or blocked, validation complete,
published, integrated, stopped, or handed off. Do not mimic minute-by-minute
AIH remote state updates. The record should make actual progress, ownership,
blockers, and the next available independent task visible without claiming an
unearned recovery or throughput guarantee.
