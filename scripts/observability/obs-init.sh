#!/usr/bin/env bash
# Creates the Grafana admin password once, in .devnet/observability. The folder is
# readable by its owner only (700). The file itself is 644, because Grafana reads it as
# its own user inside the container; the folder keeps everyone else on this machine out.
# Never prints the password. With --reset, deletes the folder.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
dir="$root/.devnet/observability"
file="$dir/grafana-admin.txt"

if [ "${1:-}" = "--reset" ]; then
  case "$dir" in
    */.devnet/observability) rm -rf "$dir" ;;
    *) echo "obs-init: refusing to delete $dir"; exit 1 ;;
  esac
  echo "obs-init: removed .devnet/observability"
  exit 0
fi

mkdir -p "$dir"
chmod 700 "$dir"
if [ ! -f "$file" ]; then
  (umask 077 && openssl rand -hex 24 > "$file")
  chmod 644 "$file"
  echo "obs-init: created the Grafana admin password"
fi
mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }
[ "$(mode "$dir")" = "700" ] || { echo "obs-init: $dir has mode $(mode "$dir"), want 700"; exit 1; }
[ "$(mode "$file")" = "644" ] || { echo "obs-init: $file has mode $(mode "$file"), want 644"; exit 1; }
echo "obs-init: ready"
