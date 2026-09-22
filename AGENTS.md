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
Run `go test ./...`, `go vet ./...`, and the release cross-build before delivery.
