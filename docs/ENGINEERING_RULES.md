# Engineering rules

The embedded, versioned core lives in `internal/roles`. It emphasizes understanding
before editing, the simplest complete design consistent with the repository,
surgical scope, observable tests, compatibility, explicit errors, meaningful names,
and low-risk assumptions recorded as decisions. It rejects speculative abstractions,
unrelated refactors and weakening tests to achieve a pass.

No universal line-count limits or mandatory lint ecosystem are imposed. Complexity
is a review signal, not a mechanical excuse to split cohesive code.

Repository AGENTS files contain only project-specific architecture, commands,
invariants and conventions. Nested instructions apply by task area/changed path.
Universal rules are not copied into each repository. Canonical main, not a stale
task worktree, supplies current instruction content.

The authority boundary is explicit: models advise/implement; the supervisor owns
commits, pushes, issues, checkpoints, retries, checks and integration. Production,
credential, billing, destructive and consequential ambiguous decisions become
task-local human blockers. These rules are not a substitute for OS isolation.

Run metadata records the rule hash and runtime version. Required verification
evidence records both the rules and canonical configuration hash, invalidating
stale evidence before integration. `aih rules` prints compiled context, and
`aih rules doctor` finds local drift. It does not promise semantic detection of
every contradiction in natural-language instructions.

Reusable harness improvements are recorded with `aih improve "observation"`.
They become portable improvement candidates, not permission to mutate the central
harness. Maintainers can promote them to tested/reviewed changes and tagged releases.
