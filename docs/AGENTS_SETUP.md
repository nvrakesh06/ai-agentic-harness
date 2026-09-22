# Coding-agent setup

Install **one** provider per project, as the OS account that will run AIH. Neither
AIH setup script installs an agent or logs you in. Provider usage can incur charges;
check your account's access, billing and spending limits before a live objective.

AIH uses fresh non-interactive processes, not provider conversation recovery.
Run `aih doctor --machine --provider codex` or `--provider claude-code` after setup.
Doctor checks CLI capabilities and authentication status, not model entitlement,
quota, sandbox viability, or a successful paid request.

## Codex

Follow the [official installation guide](https://learn.chatgpt.com/docs/codex/cli).
For Linux/macOS, download the official installer, inspect it, then run it:

```sh
provider_installer=$(mktemp)
curl -fsSL https://chatgpt.com/codex/install.sh -o "$provider_installer"
less "$provider_installer"
sh "$provider_installer"
codex --version
```

Add the installer's reported directory to PATH, including the PATH used when
generating your systemd service. Alternatively set `CODEX_BINARY` to the installed
absolute executable path. Windows users can use the official Windows installer,
or the supported npm installation (`npm install -g @openai/codex`) with a supported
Node runtime. AIH itself does not need Node; do not install it solely for AIH.

### Headless authentication

```sh
codex login --device-auth
codex login status
```

Open the displayed link/code on another device; no browser is required on the
VM. Device login may need enabling in account/workspace settings. For API billing,
provide a user-owned key through stdin, not a command-line argument (Bash):

```bash
read -r -s -p 'OpenAI API key: ' OPENAI_API_KEY; printf '\n'
export OPENAI_API_KEY
printenv OPENAI_API_KEY | codex login --with-api-key
unset OPENAI_API_KEY
```

Use a persistent credential store available to the service user. On a headless
machine without a keyring, set `cli_auth_credentials_store = "file"` in
`~/.codex/config.toml`; protect `~/.codex/auth.json` like a password. A fallback
browser callback can be tunneled using `ssh -L 1455:localhost:1455 USER@VM`, followed
by `codex login` in that SSH session. See [official authentication guidance](https://learn.chatgpt.com/docs/auth).

### AIH invocation

The adapter uses `codex exec --json`, `--output-schema`, `--output-last-message`,
`--sandbox read-only` or `workspace-write`, and approvals set to `never`.
Canonical instructions arrive on stdin; automatic project-document injection is
disabled. Keep installation-specific model/provider settings compatible with this
contract. Do not enable unrestricted/YOLO permissions to bypass a sandbox failure.

## Claude Code

Use the [official installation instructions](https://code.claude.com/docs/en/setup).
The native Linux/macOS installer does not require Node:

```sh
provider_installer=$(mktemp)
curl -fsSL https://claude.ai/install.sh -o "$provider_installer"
less "$provider_installer"
bash "$provider_installer" stable
export PATH="$HOME/.local/bin:$PATH"
claude --version
```

A stable release must still provide AIH's required flags; doctor reports missing
capabilities. Windows native installation is documented on that same page.
Set `CLAUDE_BINARY` if the executable is not on PATH. Never put CLI flags inside
the binary variable.

### Headless authentication

```sh
claude auth login
claude auth status
```

Open the displayed login URL on your own browser. If it returns a code instead
of reaching the callback, paste that code into the SSH terminal when prompted.
Linux login credentials normally live in `~/.claude/.credentials.json`. See
[Claude authentication](https://code.claude.com/docs/en/authentication).

For automation, a user-owned `ANTHROPIC_API_KEY` or a subscription token from
`claude setup-token` may be supplied in a protected environment file using
`ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`. `setup-token` prints the token;
do not record/share that terminal or paste the token into shell history, source,
or this project's issues. See [CLI reference](https://code.claude.com/docs/en/cli-reference)
and [environment variables](https://code.claude.com/docs/en/env-vars).

Choose one authentication method deliberately. Use `aih --env-file PATH doctor`
to check the actual AIH environment; an exported SSH-session variable does not
automatically reach a rebooted systemd service. AIH never refreshes or rotates
credentials itself; provider login/expiry behavior remains the provider's concern.

### AIH invocation

Claude runs with `--print --safe-mode --output-format json --json-schema ...`,
`--permission-mode dontAsk`, and `--no-session-persistence`. Advisory roles get
Read/Glob/Grep; the implementer additionally gets Edit/Write. No shell tool is
enabled. The supervisor runs all configured native checks. Safe mode is kept;
we do not substitute `--bare`, whose authentication/configuration behavior differs.

## Operating safely

Run a small, bounded live task in a disposable GitHub application after doctor.
The repository's test suite uses fake agents and does not prove every installed
provider version/model works. Record `codex --version` / `claude --version` when
reporting problems. After a CLI upgrade, rerun doctor before resuming services.

Use provider credentials only in that dedicated user's login store/environment;
never in portable `.aih` project policy. Do not copy your whole laptop home to
the VM. Authentication files and provider logs are not AIH recovery state.
See [SECURITY.md](../SECURITY.md) for the actual OS trust boundary.
