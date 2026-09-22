#!/bin/sh
set -eu
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
if [ ! -x "$repo_root/bin/aih" ]; then
    echo 'Build first with scripts/setup.sh, or use an installed aih doctor.' >&2
    exit 1
fi
# Preserve the caller's directory, so --repo and --env-file are unambiguous.
exec "$repo_root/bin/aih" doctor "$@"
