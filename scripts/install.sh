#!/bin/sh
set -eu
version=1.0.0
source_binary=
repository=${AIH_RELEASE_REPO:-nvrakesh06/ai-agentic-harness}
install_dir="${AIH_BIN_DIR:-$HOME/.local/bin}"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --source) source_binary=$2; shift 2 ;;
        --version) version=$2; shift 2 ;;
        --directory) install_dir=$2; shift 2 ;;
        --repository) repository=$2; shift 2 ;;
        *) echo "Usage: install.sh [--source binary] [--version 1.x.y] [--directory path] [--repository owner/repo]" >&2; exit 2 ;;
    esac
done
printf '%s\n' "$version" | LC_ALL=C grep -Eq '^1\.[0-9]+\.[0-9]+$' || { echo 'Only stable V1 releases are accepted.' >&2; exit 1; }
printf '%s\n' "$repository" | LC_ALL=C grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || { echo 'Invalid release repository.' >&2; exit 1; }
case "$repository" in *..*) echo 'Invalid release repository.' >&2; exit 1 ;; esac
command -v pgrep >/dev/null 2>&1 || { echo 'Install pgrep (Ubuntu: procps) to check for running supervisors.' >&2; exit 1; }
if pgrep -x aih >/dev/null 2>&1; then echo 'Stop all AIH supervisors before installing.' >&2; exit 1; fi
scratch=$(mktemp -d "${TMPDIR:-/tmp}/aih-install.XXXXXXXX")
pending=
cleanup() {
    if [ -n "$pending" ]; then rm -f -- "$pending"; fi
    rm -f -- "$scratch/binary" "$scratch/SHA256SUMS"
    rmdir "$scratch"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
if [ -z "$source_binary" ]; then
    case "$(uname -s)" in Darwin) target_os=darwin ;; Linux) target_os=linux ;; *) echo 'Unsupported OS' >&2; exit 1 ;; esac
    case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo 'Unsupported architecture' >&2; exit 1 ;; esac
    asset="aih_${target_os}_$arch"
    base="https://github.com/$repository/releases/download/v$version"
    curl --fail --location --proto '=https' --tlsv1.2 "$base/$asset" -o "$scratch/binary"
    curl --fail --location --proto '=https' --tlsv1.2 "$base/SHA256SUMS" -o "$scratch/SHA256SUMS"
    expected=$(awk -v asset="$asset" '$2 == asset {print $1}' "$scratch/SHA256SUMS")
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$scratch/binary" | awk '{print $1}')
    else
        actual=$(shasum -a 256 "$scratch/binary" | awk '{print $1}')
    fi
    if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
        echo 'Release checksum mismatch.' >&2
        exit 1
    fi
    source_binary="$scratch/binary"
fi
mkdir -p "$install_dir"
pending=$(mktemp "$install_dir/.aih-new.XXXXXXXX")
cp "$source_binary" "$pending"
chmod 755 "$pending"
if [ -f "$install_dir/aih" ]; then cp -p "$install_dir/aih" "$install_dir/aih.previous"; fi
mv -f "$pending" "$install_dir/aih"
echo "Installed $install_dir/aih. Add $install_dir to PATH, then run aih install."
