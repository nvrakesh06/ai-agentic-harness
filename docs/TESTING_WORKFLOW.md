# Testing workflow for AIH development

Separate fast implementation feedback from complete supervisor acceptance. Tests
still prove behavior; avoiding duplicate work must not become a way to skip it.
This workflow belongs to AIH and applies across consuming repositories through
their canonical check configuration, rather than adding product-specific runners.

## Implementation feedback

Read the changed contract and its existing tests. Run the relevant package or named
fixtures, including negative cases for security, recovery, and validation changes.
Use explicit anchored `-run` patterns when only pure fixtures may run alongside an
active product. Record the actual command, source revision, result, and unexecuted
checks. Do not describe a focused pass as a full-suite pass.

For example, model-only work can use `go test ./internal/model -count=1`.
`go test ./... -run '^$'` checks compilation without executing the named inventory;
it does not verify lifecycle, real Git, subprocess, browser, or recovery behavior.
`go run ./cmd/checkfmt` checks formatting. Choose tests from their actual behavior,
not just a name that sounds inexpensive. Avoid new build tags or environment skips
that silently exclude required causal fixtures from the normal release inventory.

Do not run a full release from each implementer or reviewer. Use existing shared
machine admission for real Git/helper/browser and other heavy checks. A worker
cannot infer that capacity is available merely because its own worktree is idle.
Independent pure checks may proceed while the accepted consumer keeps running.

## Supervisor acceptance

The supervisor runs the canonical applicable checks on the exact candidate head.
Existing focused-validation policy remains conservative: an eligible low-risk
single-package Go change may receive focused feedback; merge/release acceptance
still requires its configured complete gate. Changed source, config, rules,
toolchain, or test inputs invalidate old evidence under the existing rules.

For AIH delivery, `go run ./cmd/release --capacity-wait 30m` executes the complete
discovered Go package/named-test inventory, then vet and all five cross-builds.
Use the documented real-browser setup in `docs/RELEASES.md` so mandatory browser
fixtures execute. Inventory alone proves planning, not passing execution. Validate
terminal test/package events, required browser results, and final build artifacts.
There is no need to rerun a separate identical full suite immediately before this
complete release gate unless a failure, new source, or unresolved concern requires it.

Cooperative handoffs release capacity only after owned work finishes. Invocation
progress reuses its discovered plan and completed cursor while reattesting the full
immutable identity after reacquisition. A fresh invocation discovers inventory
again; mandatory browser fixtures rerun. Failed or interrupted groups neither
advance the cursor nor receive success receipts. Other durable receipt reuse remains
limited to the existing exact-identity rules.

## Evidence and measurement

Successful native checks may explicitly opt into `artifacts: true` and write PNGs
to the supervisor-provided `AIH_CHECK_ARTIFACTS` directory. Retained receipts bind
check and source provenance. They do not confer visual approval: reviewers still
need validated image handoff and inspection. An unavailable capture is an evidence
production problem, not evidence that a source repair is necessary. Retention,
resolver, handoff, recovery routing, and consumer adoption have separate rollout
requirements described in `docs/NATIVE_CHECK_ARTIFACTS.md`.

Measure useful execution, admission wait, discovery/identity bookkeeping, and
recovery/repeated checks separately. Report observed process-count or wall-time
changes at a pinned revision. A faster pure test or fewer Git starts is not a
measured end-to-end throughput gain. Keep progress durable at meaningful boundaries
without adding remote publications for every timing sample.
