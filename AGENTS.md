# AIH-specific conventions

AIH is a local software-development control plane, not a coding model.
Keep Codex and Claude invocation details inside the provider package.
Use one operational SQLite database per project and machine, outside source worktrees.
Remote state lives only on `aih-state`; never merge that branch into code branches.
State schema changes require explicit migrations and recovery tests.
Cross-machine reconstruction must work without provider conversation history.
In supervised mode, only the supervisor publishes checkpoints, creates issues/PRs,
or integrates code. Supervised integration publishes verified code and state in a
single fenced transaction.
The optional coordinator-led lightweight workflow in
`docs/LIGHTWEIGHT_WORKFLOW.md` is separate: its coordinator may use ordinary
Git/GitHub publication, never writes `aih-state`/SQLite/receipts, and does not
claim equivalent recovery guarantees. Do not run or resume both modes for one
project concurrently.
Never depend on paid GitHub features or GitHub Actions.
Run `go test ./...`, `go vet ./...`, and the release cross-build before delivery
for executable/runtime changes. For documentation-only changes, validate links,
examples, and the diff unless a repository rule makes a broader check applicable.
