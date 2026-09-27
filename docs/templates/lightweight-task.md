# Lightweight task handoff

Use one copy per independently dispatched task. This is a coordinator record for
ordinary Git/GitHub work, not AIH state. Keep session JSONL and final-message
files outside the source worktree.

## Identity and lifecycle

- Coordinator:
- Task ID and concise title:
- Mode: `lightweight`
- Status: `ready | active | committed | in review | blocked | validated | published | integrated | stopped | handed off`
- Lifecycle timestamp and note:
- Existing supervised task/branch/PR reference, if transitioning:

## Ownership and isolation

- Implementer (sole writer):
- Implementer model/policy tier:
- Worktree (absolute path):
- Branch:
- Base branch and exact SHA at dispatch:
- Owned paths/change domain:
- Read-only dependencies (task, SHA/contract, and why):
- Known conflict domains or generated outputs:
- Backfill eligibility once this task completes:

## Outcome

- User-visible/objective outcome:
- In scope:
- Explicitly out of scope:
- Known blocker and decision owner:
- Risk: `low | medium | high | consequential`

## Acceptance and evidence

- Acceptance criteria:
- Focused checks while editing:
- Required repository checks before merge:
- Heavy validation lane and bound:
- Expected evidence (commands, exit results, artifacts):
- Security review needed? Why or why not:
- QA review needed? Why or why not:
- UI review needed? List actual screenshots/states to inspect:
- Animation review needed? List rendered frames/states to inspect:
- Architecture decision needed? State the consequential decision:

## Session record

- Native CLI command model placeholder:
- Provider session ID:
- JSONL log path outside source:
- Last-message path outside source:
- Start/end/timeout result:
- Resume/cancellation/termination action taken through supported CLI behavior:

## Commit and review packet

Complete this section only after the implementer commits.

- Candidate commit SHA:
- Exact diff range (for example, `origin/main...<SHA>`):
- `git status --short` result:
- Commands actually run and results:
- Evidence/artifact locations:
- Reviewer (distinct, read-only session):
- Reviewer model/policy tier:
- Reviewer worktree at exact SHA:
- Review scope: introduced defects plus consequential relevant inherited blockers:
- Review findings (commit/diff, file/line, severity, impact, remedy):
- Repair commit(s) and re-run evidence:
- Final review disposition at exact SHA:

## Publication and integration

- Branch push/PR URL:
- Fresh-`main` integration worktree and base SHA:
- Fresh-main validation and affected-interaction checks:
- Ordinary Git/GitHub merge method and coordinator:
- Merged `main` SHA:
- Post-merge interaction recheck:
- Final blocker or handoff needed:

## Transition back to supervised mode (if applicable)

- Lightweight tasks stopped/reconciled:
- Legacy branches and AIH state preserved unchanged:
- Supported adoption action/owner (never SQL or `aih-state` edits):

