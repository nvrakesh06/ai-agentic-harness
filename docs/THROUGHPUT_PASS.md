# Bounded throughput improvement pass — 2026-09-26

## Decision and observed baseline

Pause StatMotion admissions while repairing the shared AIH control plane. Keep
its source checkpoints, user edits, and stopped state intact. Resume product work
after this pass is integrated and its public recovery interfaces are verified.
All orchestration changes belong in AIH, not the consuming product.

The stopped StatMotion snapshot (revision 4894) contains seven already-planned
accepted objectives. An additional backlog dispatcher would not unblock them.
The renderer task records 151 provider invocations: 34 implementation runs and
116 specialist/review/guidance runs, plus one advisor invocation. Recorded provider
duration totals 44,590 seconds. This is cumulative provider work, not elapsed
project time: roles can overlap and interrupted runs currently undercount cost.
The observation is evidence of repeated gates, not proof that any individual
review was unnecessary. Do not present these totals as utilization or a delivery ETA.

Existing mechanisms already provide bounded writers, isolated worktrees,
continuous dispatch, accepted-objective backfill, concurrent independent readers,
scope fencing, exact-head evidence reuse, and a merge train. Reuse them.

## Dependency DAG and ownership

```mermaid
flowchart LR
  B[Existing ten-PR batch: reviewed source candidate] --> C
  R[94 and 3: reader budgets and review waves] --> C[Combined candidate]
  N[Transient native checks: one supervisor retry] --> C
  M[88: transition accounting and run provenance] --> C
  T[45: complete bounded release test inventory] --> C
  C --> O[Operational fixtures and scoped review]
  O --> G[One combined full release gate]
  G --> I[Integrate exact verified tree]
  I --> A[Build and verify selected AIH binary]
  A --> P[Preview then apply explicit product scope recovery]
  P --> S[Resume independent StatMotion tasks]
```

The three delegated implementation tasks and the supervisor's release fix use
separate worktrees. They may write concurrently;
heavy validation is serialized through the existing resource model. Native retry
owns check handling, review work owns role dispatch, and metrics owns run-record
creation/completion and transition accounting. Integrate frequently and resolve
shared-file boundaries explicitly. The frozen release candidate is never edited.

## 1. Bounded readers and useful review ordering — issues 94 and 3

Add strict optional planning, preflight, and review time budgets while retaining
the legacy worker budget as the compatibility fallback. Implementation retains
its current soft-deadline checkpoint behavior.

Independent reviewers remain concurrent. QA that evaluates peer artifacts runs
after the other required roles and receives their attributable exact-head results.
Every required acceptance disposition remains mandatory. Neither a failure nor
an authentication error may disappear behind scheduling changes. A reader deadline
must preserve the source and evidence and must not consume a code FIX or Advisor
cycle. Any retry is explicitly bounded; otherwise expose a recoverable blocker.

Acceptance: a fake-provider/real-Git fixture makes QA assert that reviewer and
security artifacts are present; each role runs only as required. A slow reader is
bounded by its configured stage budget while unrelated eligible work can proceed.
Invalid budget values fail policy validation. Existing implementer checkpoint,
authentication, evidence identity, and reuse tests remain valid.

## 2. Transient native failures without code regeneration

Distinguish typed native-check deadlines and narrowly recognized Windows package
installation locks from source failures. Retry once through the existing native
verification queue and heavy permit at the same source/policy/check identity.
Do not invoke an implementer or Advisor and do not consume their retry budgets.
A repeated transient failure preserves diagnostics and blocks for environment
repair with verification-only recovery. Global cancellation never retries.
An assertion/compiler failure retains the existing bounded source FIX path.

Acceptance: timeout and narrowly matched lock fixtures show zero writer/Advisor
invocations and no FIX increment; second failure blocks; changed identity permits
a fresh bounded attempt; near-miss text and actual source failures do not enter
the transient lane. Diagnostics remain bounded and redacted.

## 3. Honest throughput accounting — issue 88

Record source/base/policy/stage identity on new provider runs and finalize duration
on interruption. Account for task-state changes at meaningful state publications,
including direct state assignments. Identical observations must not create new
portable state commits. Expose a read-only human/JSON report distinguishing
provider work, recorded state residence, blockers, and repeated runs at an exact
head. Historical missing measurements are unavailable, not fabricated zeros.
Paused time and operator wait cannot be labeled productive or avoidable idle.

Acceptance: deterministic clocks prove transition accounting and interrupted-run
duration; round-trip/migration preserves legacy snapshots; repeated unchanged-head
review is visible; no-op persistence stays a no-op. Report cumulative provider
seconds separately from elapsed time and do not add overlapping roles into wall time.

## 4. Complete bounded release inventory — issue 45

The frozen `9278fb4` gate failed at the engine package's 900-second aggregate
deadline. It had completed 180 top-level tests, totaling 880.53 recorded seconds,
and was still advancing through scope recovery fixtures. No earlier assertion
failure occurred. This is failed validation, not permission to merge the batch.

Discover all packages and runnable engine tests through Go's native inventory,
then execute every engine test in deterministic serial groups of sixteen. Preserve
uncached execution, fail-fast behavior, fixture deadlines, and each group's
fifteen-minute bound. Require a terminal pass/skip event for every planned test.
Record the complete planned inventory; never imply that unexecuted groups passed.
Tests prove exact-once grouping, rejection of incomplete/unsafe inventories, and
failure when a subprocess exits successfully without covering a planned test.

## Completion gate and resumption

Use focused checks on each changed contract, then scoped independent review. Run
one full uncached combined release gate with vet and cross-builds after operational
fixtures pass. Do not duplicate the full suite per branch. Record exact commit/tree
identities and limitations. Merge normal source history without bypassing fencing
or weakening required evidence. Then build the selected binary, run the read-only
scope recovery preview against current product state, apply through supported AIH
interfaces, and resume dependency-unblocked tasks with the configured safe writers.

Report product throughput as verified merged changes and critical-path age after
resumption. More sessions, open issues, or checkpoint commits are not success.

## Deferred unless a demonstrated blocker remains

General stack-PR machinery, automatic binary draining, configurable review DAGs,
external shell resource reservation, and a broader task-contract rewrite are not
part of this pass. Broader Windows fixture resource lanes require a demonstrated
fixture-contention failure beyond the observed aggregate timeout. Do not create
more infrastructure simply because an audit proposed it.
The existing batch already repairs publication timeout recovery, deadlock,
history-preserving synchronization, focused/shared validation, sealed visual
reattestation, provider-auth handling, and explicit scope/task recovery.
