# Security policy

## Reporting a vulnerability

Use this repository's **Security → Report a vulnerability** private reporting
channel, or [open a private advisory](https://github.com/nvrakesh06/ai-agentic-harness/security/advisories/new).
Do not publish exploit details, tokens, private source, or personal data in an
issue/PR. If private reporting is unavailable (for example on a fork), ask a
maintainer to arrange a private channel without disclosing the vulnerability.

Include affected versions/platforms, impact, a minimal sanitized reproduction,
and any suggested mitigation. Maintainers handle reports on a best-effort basis;
there is no paid response SLA. Fixes target current V1/main; older snapshots have
no separate security-maintenance guarantee. Coordinate public disclosure after
a mitigation is available.

If a credential was published, **revoke/rotate it first**. Deleting a file or
rewriting Git history alone cannot revoke credentials or recall existing clones.

## Execution boundary

AIH runs repository-controlled code. It is **not safe for untrusted repositories,
untrusted PRs, or multi-tenant execution under one OS account**. Instructions and
agent permissions are not an OS security boundary against malicious code.

- Codex uses `read-only` or `workspace-write` and non-interactive approvals; it
  can execute shell tools under the provider's sandbox/configuration.
- Claude uses safe mode, `dontAsk`, and a restricted read/edit tool list. AIH
  does not enable a Claude shell tool or permission-bypass flag. Managed provider
  policy can still affect execution; audit local/managed settings.
- Native verification commands run **without a harness sandbox**, with the
  application user's OS permissions. Tests, build scripts and dependencies can
  read that user's credential files, access networks and spawn processes.
- GitHub token environment variables are removed from provider invocations;
  credential-like environment names are filtered from native checks. This does
  not block filesystem access to credentials, every secret format, or all indirect
  credential mechanisms. Secret scanning/redaction is heuristic.
- The same supervisor user must have GitHub write access and provider credentials.
  A compromised application therefore threatens those credentials and repositories.

Run as a dedicated **non-root** account on a dedicated VM or development machine.
Do not grant passwordless sudo, mount a Docker socket, use a privileged container,
forward a broad SSH agent, or keep production/cloud-administrator credentials in
that account. Grant only necessary repository permissions, enforce spending limits,
monitor activity, and keep the OS, Git, agents and build tools patched.

## Data and publication

Objectives, summaries, findings, decisions and verification evidence are published
to application Git/GitHub. With a public application repository this is public
data. Never submit private material unless its repository/provider sharing policy
permits it. AIH does not change application repository visibility.

Keep AIH_HOME and provider authentication outside source on a local persistent
disk. Use restrictive file permissions and encrypted backups/disks where needed.
Logs, result files, raw provider transcripts, and SQLite commands can contain
sensitive source/data even when known token formats are redacted. Provider tools
may keep their own logs outside AIH_HOME. Inspect before sharing diagnostics.

The runtime exposes **no network listener**. Access is through the local CLI/SSH;
do not expose it through an unauthenticated web wrapper. Verify SSH host keys and
keep Git TLS verification enabled. Never solve ownership errors with
`safe.directory=*`. No script changes global Git identity or disables protections.

AIH's remote lease and atomic pushes fence cooperating controllers, not hostile
repository writers. Repository admins can edit state and refs. Main/state ancestry
must advance, and rejected atomic publication fails closed. Local queued commands
and unpushed edits can be lost with the disk; follow the recovery/backup runbook.

Release checksums detect corruption, not a compromised publisher. Source installs
use Go's checksums; review dependency and provider updates. Release binaries are
not signed/notarized and no provenance-attestation infrastructure is claimed.
