# Git and GitHub setup

AIH supports `github.com`, a canonical `main` branch, issues and pull requests.
Authenticate under the same OS user that runs the supervisor. Your application's
origin comes from its own Git configuration, not the harness's upstream URL.

## Recommended V1: GitHub CLI and HTTPS

```sh
gh auth login --hostname github.com --git-protocol https --web
gh auth setup-git
gh auth status --hostname github.com
git clone https://github.com/YOUR_OWNER/YOUR_APPLICATION.git
cd YOUR_APPLICATION
git ls-remote origin refs/heads/main
```

The web/device flow works by opening the printed URL/code on another device.
`gh auth setup-git` intentionally configures this user's Git credential helper;
AIH's bootstrap does not run it implicitly. On a headless machine without a
credential store, gh may save a plaintext token in its own config directory;
protect that account and disk. [GitHub CLI authentication reference](https://cli.github.com/manual/gh_auth_login).

For token-based automation, provision a repository-scoped credential separately
and supply `GH_TOKEN` through the service's protected environment file, never a
script or URL. Ensure both `gh api` and Git HTTPS operations use the credential
helper. Fine-grained access needs repository metadata read plus contents, issues,
and pull requests read/write; organization approval/SSO and repository rules may
also apply. A GitHub App/token broker is not required or implemented in V1.

Use a dedicated account or narrowly scoped token; avoid giving a VM every
repository permission your personal account has. Do not print `gh auth token`
into logs. Never use `https://TOKEN@github.com/...` remotes; AIH rejects embedded
credentials.

## SSH alternative

AIH accepts `git@github.com:OWNER/REPO.git` and
`ssh://git@github.com/OWNER/REPO.git`. Configure an SSH key and verified host key
for the service account, then test non-interactive Git access. Compare the host
key with [GitHub's published fingerprints](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints)
before accepting it. Do not disable host verification or blindly trust keyscan.

SSH Git access does **not** authenticate GitHub's API: gh still needs login/token
access for issues and PRs. A key that only exists in your laptop's forwarded
ssh-agent will stop working when you disconnect. Prefer HTTPS/gh for the simple
boot-independent deployment. AIH strips `GIT_*` environment overrides; put SSH
host/key choices in the account's protected SSH config instead.

## Identity, ownership and policy

For your human setup commits, set identity locally in the application:

```sh
git config user.name "Your Name"
git config user.email "YOUR_VERIFIED_OR_NOREPLY_EMAIL"
```

AIH-generated commits use `AIH <aih@localhost>` via per-command configuration,
disable signing and Git hooks for supervisor operations, and do not change your
global identity. No personal developer identity is baked into task commits.

Clone as the application user; don't clone as root and then run as another user.
Fix ownership when necessary. Do not set `safe.directory=*`; if a deliberately
shared checkout needs trust, configure only its exact path after inspection.

AIH integrates a locally verified merge commit by atomically advancing `main`,
the task branch and `aih-state` with explicit expected revisions. PRs retain
evidence and merge ancestry. Repository rules that forbid direct updates or
require another merge method can reject this workflow. AIH does not relax those
rules: choose a repository policy compatible with the documented V1 workflow,
or do not enable autonomous integration there. Doctor does not test push policy
by making an empty/dummy publication.

Keep `aih-state` separate from code; do not delete it or squash/rewrite it.
Never run controllers on two machines expecting a distributed queue: only one
owns the live lease. See [handoff/recovery](RECOVERY.md).
