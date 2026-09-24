# Ubuntu VM deployment

Primary server target: **Ubuntu 24.04 LTS**, amd64. Linux arm64 builds are provided
but need native validation on your chosen host. Start with **2 vCPU, 4 GiB RAM and
30 GiB persistent SSD** for a small application; this is a starting estimate, not
a workload guarantee. Builds and three concurrent writers can require much more.
Set `max_parallel_writers: 1` initially for a small VM. Keep clocks synchronized.

AIH is one foreground supervisor per application, so **systemd user services**
are sufficient. No Docker, database service, web port, Kubernetes, or reverse proxy
is required. Outbound HTTPS is needed for GitHub, providers, and dependencies;
inbound access can be limited to SSH from trusted addresses. SSH keys, firewall
rules, OS updates and cloud billing remain your responsibility.

## 1. Prepare a fresh server

As your cloud administrator (not the eventual application account):

```sh
sudo apt-get update
sudo apt-get install -y git gh golang-go ca-certificates curl less procps \
  openssh-client tmux dbus-user-session
sudo adduser --disabled-password --gecos '' aih
sudo loginctl enable-linger aih
```

Provision an SSH public key for `aih` using your cloud's normal access process,
or append your trusted public key as the administrator:

```sh
sudo install -d -m 700 -o aih -g aih /home/aih/.ssh
# Replace this placeholder with ONE complete line from your own .pub file.
# Never paste a private key here. Appending preserves any existing authorized keys.
printf '%s\n' 'REPLACE_WITH_YOUR_COMPLETE_SSH_PUBLIC_KEY' | \
  sudo tee -a /home/aih/.ssh/authorized_keys > /dev/null
sudo chown aih:aih /home/aih/.ssh/authorized_keys
sudo chmod 600 /home/aih/.ssh/authorized_keys
```

Then **SSH directly as aih** (`ssh aih@YOUR_VM_ADDRESS` from your own computer).
Do not grant this account sudo or Docker-group access. Direct login creates the
correct systemd user session; `sudo su` alone may not create the user bus.

Ubuntu's Go package bootstraps the newer pinned toolchain automatically. Keep
`GOTOOLCHAIN` unset or `auto`; use your organization's approved Go distribution
if automatic downloads are prohibited. If apt reports checksum errors, retry
after refreshing its metadata/cache; do not disable signature/hash checking.

## 2. Clone and install AIH

All remaining commands run as `aih` unless explicitly marked administrator:

```sh
mkdir -p "$HOME/src" "$HOME/apps" "$HOME/.config/aih"
chmod 700 "$HOME/.config/aih"
git clone https://github.com/nvrakesh06/ai-agentic-harness.git "$HOME/src/ai-agentic-harness"
cd "$HOME/src/ai-agentic-harness"
./scripts/setup.sh
sh scripts/install.sh --source ./bin/aih
export PATH="$HOME/.local/bin:$PATH"
cp .env.example "$HOME/.config/aih/aih.env"
chmod 600 "$HOME/.config/aih/aih.env"
aih version
aih demo
```

Add the PATH export to your own shell profile if desired; no script modifies it.
The service generator captures the current PATH, so include any application
runtime/version-manager paths before generating the unit. Use absolute paths
for provider overrides if you prefer stable locations.

Edit `~/.config/aih/aih.env` only if overriding defaults. It is loaded explicitly
by AIH, **not sourced as shell code**. It does not expand `$HOME` or `~`; use
literal absolute paths. Default state is `/home/aih/.aih` for this example account.
Use a local persistent disk, not tmpfs, a network share or a synced laptop folder.

## 3. Authenticate external tools

```sh
gh auth login --hostname github.com --git-protocol https --web
gh auth setup-git
```

Open GitHub's printed URL/code on another device. Install/authenticate one agent
using [AGENTS_SETUP.md](AGENTS_SETUP.md), which includes the exact installer and
headless commands. For Codex: `codex login --device-auth`; for Claude:
`claude auth login` (open its URL elsewhere and paste a code if prompted).
Normal logins persist in this account's provider store and need no open laptop.
If using API/OAuth environment credentials instead, put only those required in
the protected environment file, not a script or unit file.

```sh
aih --env-file "$HOME/.config/aih/aih.env" doctor --machine --provider codex
# For Claude: replace codex with claude-code.
```

Every `[FAIL]` needs attention. `--offline` skips authentication/remote checks
and is **not** sufficient for server readiness. Do not assume logged-in means
the account has quota or every requested model entitlement.

## 4. Clone and configure the application

Replace `YOUR_OWNER/YOUR_APPLICATION` with a repository you trust and can write:

```sh
git clone https://github.com/YOUR_OWNER/YOUR_APPLICATION.git "$HOME/apps/my-app"
cd "$HOME/apps/my-app"
aih --env-file "$HOME/.config/aih/aih.env" attach
```

That path assumes AIH was already enabled and configuration is on origin/main.
For a **new** application, instead of attach:

```sh
aih --env-file "$HOME/.config/aih/aih.env" init --provider codex
# Review .aih/project.yaml and AGENTS.md; install application build tools.
git config user.name "Your Name"
git config user.email "YOUR_VERIFIED_OR_NOREPLY_EMAIL"
git add .aih AGENTS.md
git commit -m "Enable AIH"
git push origin main
```

Use `--provider claude-code` if that is your selected agent. AIH never installs
application dependencies automatically. Every verification worktree must be able
to install/build/test reproducibly; [development/configuration](DEVELOPMENT.md)
has examples. Commit Linux checks/platform guidance to canonical main.

```sh
aih --env-file "$HOME/.config/aih/aih.env" doctor
```

## 5. Install and start the service

If a detached AIH supervisor is already running here, run
`aih --env-file "$HOME/.config/aih/aih.env" stop` first.
Do not start a second manager for it. Then, in the application:

```sh
aih --env-file "$HOME/.config/aih/aih.env" service install --name my-app
systemd-analyze --user verify "$HOME/.config/systemd/user/aih-my-app.service"
systemctl --user daemon-reload
systemctl --user enable --now aih-my-app.service
systemctl --user status aih-my-app.service --no-pager
aih --env-file "$HOME/.config/aih/aih.env" status
aih --env-file "$HOME/.config/aih/aih.env" run "Implement a small feature with tests"
```

The generated unit runs the installed binary with `start --foreground`, absolute
repository/state/env-file paths, a captured PATH, private file umask, and journal
logging. It copies **no tokens** into the unit and never runs a login shell.
Installation alone does not enable/start it. Different projects need distinct names.
The generator refuses to overwrite a different existing unit; back it up before
regenerating after a binary/path change. `--print` previews without writing.

Linger starts the user's manager at boot and keeps it after logout. Check:

```sh
loginctl show-user aih -p Linger
systemctl --user is-enabled aih-my-app.service
```

Disconnect SSH: the service continues. After a deliberate VM reboot, reconnect and
check service status, lease/heartbeat and logs. A service process being active is
not proof that a task is progressing or that provider credentials are still valid.

## Start, stop, restart, and lightweight development

```sh
systemctl --user start aih-my-app.service
systemctl --user stop aih-my-app.service
systemctl --user restart aih-my-app.service
systemctl --user disable --now aih-my-app.service  # stop and disable boot startup
journalctl --user -u aih-my-app.service -n 100 --no-pager
journalctl --user -u aih-my-app.service -f
```

Stop sends SIGTERM to the supervisor, which cancels workers, checkpoints after
they exit and releases ownership when possible. The unit allows 180 seconds;
after that systemd kills the remaining cgroup. `Restart=on-failure` retries every
15 seconds, with no start-rate lockout. An intentional clean `aih stop`/`handoff`
does not auto-restart, but a still-enabled service will start on the next boot.
For a machine handoff, **disable the old service first** so it cannot compete
for ownership after reboot. Prefer `systemctl stop` for service-managed shutdown.

For a short development session, instead of systemd:

```sh
tmux new -s aih
aih --env-file "$HOME/.config/aih/aih.env" start --foreground
# Ctrl-B, then D detaches; tmux attach -t aih reconnects.
```

tmux survives SSH disconnection, not reboot. Do not run it alongside systemd for
the same project. Ctrl-C requests graceful foreground shutdown.

## Restart and outage recovery

On an unclean stop, the remote lease remains until expiry (180 seconds by default,
plus a five-second grace). Systemd's retries wait this out by attempting normal
startup; **no force takeover** is introduced. The first successful startup reads
remote state, marks unfinished runs interrupted, reschedules interrupted writers
and verification, and preserves pending human blockers and integration holds.
Retained worktrees/SQLite survive a VM reboot. Network outages can delay recovery.

A destroyed disk loses local-only queued requests, unpushed changes and local
diagnostics. Recreate the account/tools/authentication, clone the application,
run `aih attach`, inspect `aih status`, then install/start its service. Remote
acknowledged state/checkpoints are recoverable without provider transcripts.
See [RECOVERY.md](RECOVERY.md); never delete `aih-state` or override a live lease.

## Storage, logs, and backups

Default persistent state: `~/.aih/projects/<project_id>/`. It contains SQLite
(`state.db`, WAL/SHM), `control.git`, task worktrees and `sessions/<run_id>/`.
`aih logs` shows recent structured SQLite events and diagnostic paths. Systemd
captures supervisor stdout/stderr in the **journal**, not `logs/supervisor.log`;
that file is used only by detached CLI starts. Worker `output.log` and result
files stay under sessions. Provider CLIs may maintain their own credential/log dirs.

For journal retention across reboots, an administrator should verify persistent
journaling. On a VM without it:

```sh
sudo mkdir -p /var/log/journal
sudo systemd-tmpfiles --create --prefix /var/log/journal
sudo journalctl --flush
```

Configure system-wide journal size/time limits in the VM's journald settings
according to disk capacity; don't expect AIH to manage other services' logs.
Monitor `df -h`, `du -sh ~/.aih` and `journalctl --disk-usage`. V1 keeps state
history, local events and completed worktrees; it has **no automatic retention
pruner**. Budget disk, and archive old local session diagnostics only after
stopping the service and identifying what you no longer need. Never run a broad
recursive deletion of AIH_HOME or a live project to free space.

Back up the **stopped** whole AIH_HOME, application policy, environment file and
necessary credential stores to encrypted, access-controlled storage. Copying just
a live SQLite file can omit its WAL. Git protects published work, not local queues
or credentials. Do not share/restore a live machine directory to two controllers;
for cross-machine logical reconstruction prefer attach and fresh authentication.

## Updates

Stop every supervisor using the binary. For this one-service example:

```sh
systemctl --user stop aih-my-app.service
cd "$HOME/src/ai-agentic-harness"
git pull --ff-only
./scripts/setup.sh
go test -p 1 ./... -timeout 15m
sh scripts/install.sh --source ./bin/aih
cd "$HOME/apps/my-app"
aih --env-file "$HOME/.config/aih/aih.env" doctor
systemctl --user restart aih-my-app.service
```

The installer retains the previous binary. Review release notes before rollback:
old binaries can reject newer state schemas. No automatic major migration occurs.
If paths or captured PATH changed, back up/regenerate the unit and daemon-reload.
Rotated environment-file credentials are read on restart. Exported SSH values
are not persisted to systemd.

## Troubleshooting

- **User bus unavailable:** SSH directly as the application user. Verify
  `dbus-user-session`, linger, and the administrator's `user@UID.service`. Do not
  hardcode another user's `XDG_RUNTIME_DIR` or launch the service as root.
- **Lease unavailable:** normal briefly after a crash; inspect expiry and keep
  VM clocks synchronized. A genuinely live remote owner is never overridden.
- **Authentication works only in your terminal:** inspect service user, HOME,
  PATH and `--env-file` in `systemctl --user cat aih-my-app`. Login again under
  the application user; avoid forwarded agents and desktop-only keyrings.
- **Repeated exits:** inspect the journal and `aih logs`; fix missing binaries,
  permissions, disk space, configuration or network, then restart. Automatic
  restarts do not repair an invalid setup.
- **Blocked tasks:** use `aih blockers` and answer actual decisions. Do not edit
  SQLite to force DONE or clear an integration hold.
- **Git rule/permission rejection:** see [GitHub setup](GITHUB_SETUP.md). Never
  disable SSH/TLS verification or broadly trust unknown checkout owners.

See [VALIDATION.md](VALIDATION.md) for exactly what was exercised versus what
still needs a real cloud VM/provider smoke test.
