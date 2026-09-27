# AIH-specific conventions

AIH is a local software-development control plane, not a coding model.
Keep Codex and Claude invocation details inside the provider package.
Use one operational SQLite database per project and machine, outside source worktrees.
Remote state lives only on `aih-state`; never merge that branch into code branches.
State schema changes require explicit migrations and recovery tests.
Cross-machine reconstruction must work without provider conversation history.
Only the supervisor publishes checkpoints, creates issues/PRs, or integrates code.
Integration publishes the verified code and state in a single fenced transaction.
Never depend on paid GitHub features or GitHub Actions.
Read `docs/TESTING_WORKFLOW.md` before choosing validation commands.
During implementation, run the focused tests relevant to the change and report
exact commands and results. A compile-only check is not a test pass. Do not start
an overlapping full release gate or bypass the shared heavy-check permit.
The supervisor owns complete exact-head acceptance before integration/delivery:
the full Go test inventory, `go vet ./...`, mandatory real-browser fixtures, and
the release cross-build. Workers must report those checks as pending until the
supervisor supplies their results. Never weaken coverage to improve throughput.
