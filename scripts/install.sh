#!/bin/sh
set -eu
version=1.0.0
source_binary=
install_dir="${AIH_BIN_DIR:-$HOME/.local/bin}"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --source) source_binary=$2; shift 2 ;;
        --version) version=$2; shift 2 ;;
        --directory) install_dir=$2; shift 2 ;;
        *) echo "Usage: install.sh [--source binary] [--version 1.x.y] [--directory path]" >&2; exit 2 ;;
    esac
done
case "$version" in 1.*.*) ;; *) echo 'Only stable V1 releases are accepted.' >&2; exit 1 ;; esac
if pgrep -x aih >/dev/null 2>&1; then echo 'Stop all AIH supervisors before installing.' >&2; exit 1; fi
scratch=$(mktemp -d "${TMPDIR:-/tmp}/aih-install.XXXXXXXX")
trap 'rm -f "$scratch/binary" "$scratch/SHA256SUMS"; rmdir "$scratch"' EXIT HUP INT TERM
if [ -z "$source_binary" ]; then
    [ "$(uname -s)" = Darwin ] || { echo 'Release installer supports macOS; build from source elsewhere.' >&2; exit 1; }
    case "$(uname -m)" in arm64) arch=arm64 ;; x86_64) arch=amd64 ;; *) echo 'Unsupported architecture' >&2; exit 1 ;; esac
    asset="aih_darwin_$arch"
    base="https://github.com/nvrakesh06/ai-agentic-harness/releases/download/v$version"
    curl --fail --location --proto '=https' --tlsv1.2 "$base/$asset" -o "$scratch/binary"
    curl --fail --location --proto '=https' --tlsv1.2 "$base/SHA256SUMS" -o "$scratch/SHA256SUMS"
    expected=$(awk -v asset="$asset" '$2 == asset {print $1}' "$scratch/SHA256SUMS")
    actual=$(shasum -a 256 "$scratch/binary" | awk '{print $1}')
    [ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo 'Release checksum mismatch.' >&2; exit 1; }
    source_binary="$scratch/binary"
fi
mkdir -p "$install_dir"
pending=$(mktemp "$install_dir/.aih-new.XXXXXXXX")
cp "$source_binary" "$pending"
chmod 755 "$pending"
if [ -f "$install_dir/aih" ]; then cp -p "$install_dir/aih" "$install_dir/aih.previous"; fi
mv -f "$pending" "$install_dir/aih"
echo "Installed $install_dir/aih. Add $install_dir to PATH, then run aih install."
