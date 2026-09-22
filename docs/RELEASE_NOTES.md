# AIH 1.0.0

Initial local control plane: Git/SQLite durability, fenced controller ownership,
bounded Codex and Claude workers, specialist reviews, dependency scheduling,
verified atomic integration, human blockers, and cross-machine reconstruction.

Requires Git, authenticated GitHub CLI, and one authenticated supported provider.
No GitHub Actions or paid repository features are required.

Source-readiness improvements: Linux amd64/arm64 builds and installation,
cross-platform bootstrap and doctor, explicit environment files, configurable
agent paths, non-root systemd user services, headless authentication/deployment
runbooks, MIT licensing, and contribution/security policies. Local fetches are
serialized and implicit push tracking updates disabled to prevent ref races.
New recovery, storage-permission, configuration and service-rendering tests keep
the existing V1 state schema and Git publication guarantees unchanged.

Read the security boundaries and validation status in README before using this
on a real project. This release does not claim hostile-code isolation.
