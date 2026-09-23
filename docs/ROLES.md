# Roles and context

Built-ins: Orchestrator (plan), Implementer (sole writer), Reviewer, QA, Designer,
Security and Advisor. Reviewer and QA are always required for source changes.
Designer is required for UI tasks; common UI extensions also trigger it. Security
is triggered by high risk, explicit flags, or sensitive changed paths. Path
heuristics are conservative aids, not complete semantic detection.

UI tasks receive pre-implementation design guidance and post-implementation design
review. Visual checks should be configured in the application; lack of screenshots
or runnable UI evidence must not be reported as a visual pass. Advisor is reserved
for exhausted normal retry budgets and offers one bounded final approach.

## Custom advisory role

```yaml
name: market-data-validator
description: Check timestamps and stale market data
extends: reviewer
mode: validator
stage: review
permissions: [read]
capability: strong
output_schema: worker-v1
context: [docs/MARKET_DATA.md]
triggers:
  paths: ["market-data/**", "providers/**"]
  risks: [high]
independent_parent_review: false
focus:
  - timestamp correctness
  - exchange-session boundaries
blocking:
  severities: [critical, high]
instructions: Cite a concrete source location for each finding.
```

Store this in `.aih/roles/market-data-validator.yaml` and commit/push to main.
Triggers are ORed across matching paths/risks. `*` matches one path segment, `**`
matches across segments. Stages are `review` or `pre-implementation`; modes are
`validator` or `advisor`. Built-in overrides and custom writers are rejected:
specialists advise the single implementation writer instead of competing with it.
At the `review` stage, a triggered validator that extends `reviewer` or `designer`
includes the parent instructions and therefore replaces that parent's default pass.
QA and Security always remain independent. Set `independent_parent_review: true`
only when the built-in parent and the specialist must perform separate reviews; this
keeps both passes and their reader cost. A pre-implementation role never replaces a
review-stage parent.
Explicit context files must exist on canonical main and are limited to 128 KiB each.

`aih roles assign TASK_ID ROLE` manually adds a specialist to a READY, FIX, or
unmerged BLOCKED task. A role can also be assigned in the validated plan. Manual
assignment does not answer an outstanding human blocker.

## Structured result envelope

Every role uses `worker-v1` rather than arbitrary executable schemas:

```json
{
  "schema_version": 1,
  "status": "completed",
  "summary": "Public outcome, not private reasoning",
  "question": "",
  "changed_areas": [],
  "tests_run": [],
  "remaining_risks": [],
  "findings": [],
  "plan": []
}
```

Other statuses: `in_progress`, `blocked`, `failed`. A human or advisory blocker
requires a question.
Findings require severity (`critical/high/medium/low/nit`) and reason, with category,
location and suggested_resolution. Orchestrator plans are validated for readiness,
unique keys, bounded task count, known dependencies and acyclicity. Unknown fields,
trailing JSON, unsupported versions and secret-like values are rejected.

For the Implementer only, `blocked` with an empty question has a narrower meaning:
implementation is complete and remaining checks require the supervisor's canonical
native environment. AIH checkpoints the work and attempts those checks directly.
Human-decision blockers still require a question. Implementers return `failed` for
code or implementation defects so those continue through the normal fix budget.

Critical/high findings block by default. Custom roles can add blocking severities.
Medium findings create stable-ID follow-up issues grouped by owning source file.
Each observation keeps its role, location, reason and resolution in the issue, so
distinct remedies in one file remain visible. Findings without a source file are
grouped by category. Re-reviewing a head refreshes the same owner issue even when
wording changes. Low/nit findings do not create churn. Native check success does
not replace independent acceptance review.

Review roles run concurrently and decide their own acceptance domain independently.
An empty peer-review map is intentional and never a reason to wait or block. Native
check evidence includes the exact task head/config/rules plus the configured check
identity, executable basename, exit status, and stdout byte/line counts. Successful
stdout content, command arguments, and environment values do not enter portable
state or the GitHub PR. A restricted review worker's missing runtime or inability
to reproduce those checks is not itself a finding when supervisor evidence is
present. A reviewer that needs refreshed
supervisor-owned evidence requests it without asking a human to run tools; AIH
reruns checks and retries that role once. A repeated evidence request becomes a
bounded verification failure. Only consequential product, risk, credential or
irreversible decisions become human blockers. Concrete findings are preserved and
take the configured fix route even when the same result also requests evidence.

## Context isolation

The compiler combines embedded core rules, a role delta, canonical root and
applicable nested `AGENTS.md`, explicit role context, active platform instructions,
the assigned task, public decisions, diff and test evidence. It does not load every
issue or provider conversation. Reviews use actual changed paths for scoped rules.

Use `.aih/platform/windows.md`, `.aih/platform/darwin.md`, or
`.aih/platform/linux.md` for platform deltas.
Only the active platform file is injected. Keep root AGENTS platform-neutral.
Claude's safe mode and Codex's disabled automatic project-document injection keep
stale local provider instructions from replacing supplied canonical instructions.
Audit existing CLAUDE.md/Codex configuration when enabling a repository.

`aih rules --role ROLE --task TASK_ID` displays effective compiled context;
`aih rules --core` works outside a project. `aih rules doctor` reports local versus
canonical instruction/config differences and validates role configuration.
