#!/usr/bin/env bash
# Creates the bridge's passwords once, in .devnet/bridge: the PostgreSQL admin's, the
# deposit indexer's, the withdrawal payer's, the requester's (withdrawal-check), the
# auditor's (bridge-check) and those of the two operators who resolve suspense under
# four eyes (ops_a, ops_b). The folder is readable by its
# owner only (700). The files are 644, because PostgreSQL and the indexer read them as
# their own users inside their containers; the folder keeps everyone else on this
# machine out. Never prints a password. With --reset, deletes the folder.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
dir="$root/.devnet/bridge"

if [ "${1:-}" = "--reset" ]; then
  case "$dir" in
    */.devnet/bridge) rm -rf "$dir" ;;
    *) echo "bridge-init: refusing to delete $dir"; exit 1 ;;
  esac
  echo "bridge-init: removed .devnet/bridge"
  exit 0
fi

mkdir -p "$dir"
chmod 700 "$dir"
mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }
for name in postgres-admin indexer payer requester auditor ops-a ops-b; do
  file="$dir/$name.txt"
  if [ ! -f "$file" ]; then
    (umask 077 && openssl rand -hex 24 > "$file")
    chmod 644 "$file"
    echo "bridge-init: created the $name password"
  fi
  [ "$(mode "$file")" = "644" ] || { echo "bridge-init: $file has mode $(mode "$file"), want 644"; exit 1; }
done
[ "$(mode "$dir")" = "700" ] || { echo "bridge-init: $dir has mode $(mode "$dir"), want 700"; exit 1; }
echo "bridge-init: ready"
