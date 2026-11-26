#!/usr/bin/env bash
# Installs the pinned shellcheck and gitleaks for Linux x86_64 into the given folder. Each
# archive is checked against its sha256 before it is unpacked; a mismatch stops the run.
# CI uses this so it runs the same versions as the local checks (ENGINEERING.md). To
# upgrade a tool, change its version and hash here and in ENGINEERING.md together.
set -euo pipefail

SHELLCHECK_VERSION="v0.11.0"
SHELLCHECK_SHA256="8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198"
GITLEAKS_VERSION="8.30.1"
# Equal to the entry in gitleaks_8.30.1_checksums.txt of the release.
GITLEAKS_SHA256="551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb"

dest="${1:?usage: install-tools.sh <folder>}"
[ "$(uname -s)-$(uname -m)" = "Linux-x86_64" ] || { echo "install-tools: Linux x86_64 only"; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# fetch <url> <sha256> <file>: download into the temporary folder and verify.
fetch() {
  curl -fsSL --retry 3 -m 120 -o "$tmp/$3" "$1"
  printf '%s  %s\n' "$2" "$tmp/$3" | sha256sum -c --quiet - \
    || { echo "install-tools: sha256 mismatch for $1"; exit 1; }
}

mkdir -p "$dest"

fetch "https://github.com/koalaman/shellcheck/releases/download/$SHELLCHECK_VERSION/shellcheck-$SHELLCHECK_VERSION.linux.x86_64.tar.xz" \
  "$SHELLCHECK_SHA256" shellcheck.tar.xz
tar -xJf "$tmp/shellcheck.tar.xz" -C "$tmp"
install -m 755 "$tmp/shellcheck-$SHELLCHECK_VERSION/shellcheck" "$dest/shellcheck"

fetch "https://github.com/gitleaks/gitleaks/releases/download/v$GITLEAKS_VERSION/gitleaks_${GITLEAKS_VERSION}_linux_x64.tar.gz" \
  "$GITLEAKS_SHA256" gitleaks.tar.gz
tar -xzf "$tmp/gitleaks.tar.gz" -C "$tmp" gitleaks
install -m 755 "$tmp/gitleaks" "$dest/gitleaks"

echo "install-tools: shellcheck $("$dest/shellcheck" --version | awk '/^version:/ {print $2}'), gitleaks $("$dest/gitleaks" version)"
