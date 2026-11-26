#!/usr/bin/env bash
# Creates the L1 genesis (execution and consensus), the JWT secret and the validator
# keystores in .devnet/l1/genesis, once. With --reset, deletes .devnet/l1 first.
# Partial output is never reused: if a run fails, the folder is removed.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
dir="$root/.devnet/l1"
gen_image="ethpandaops/ethereum-genesis-generator:6.2.9"
values="$root/infra/devnet/l1/values.env"

remove_state() {
  # Guard: only ever delete <repo>/.devnet/l1.
  case "$dir" in
    */.devnet/l1) rm -rf "$dir" ;;
    *) echo "l1-init: refusing to delete $dir"; exit 1 ;;
  esac
}

if [ "${1:-}" = "--reset" ]; then
  remove_state
  echo "l1-init: removed .devnet/l1"
  exit 0
fi

if [ -f "$dir/genesis/metadata/genesis.json" ] && [ -d "$dir/genesis/keys/keys" ]; then
  echo "l1-init: genesis exists, reusing it"
  exit 0
fi

remove_state
mkdir -p "$dir/genesis" "$dir/geth" "$dir/beacon"
log="$dir/init.log"

# Genesis time is now plus GENESIS_DELAY. The generator default (0) dates the chain to
# 1970, and Lighthouse then refuses to start: its head state is far outside the weak
# subjectivity period.
genesis_timestamp="$(date +%s)"
if ! docker run --rm -e GENESIS_TIMESTAMP="$genesis_timestamp" \
     -v "$dir/genesis:/data" -v "$values:/config/values.env:ro" \
     "$gen_image" all >"$log" 2>&1; then
  echo "l1-init: genesis generation failed, last lines:"
  tail -20 "$log"
  remove_state
  exit 1
fi

# The validator mnemonic is the generator's public test mnemonic. It is read inside
# the container and never printed. The single quotes are deliberate: the variables expand
# inside the container, not on the host.
# shellcheck disable=SC2016
if ! docker run --rm --entrypoint sh -v "$dir/genesis:/data" -v "$values:/config/values.env:ro" \
     "$gen_image" -c '. /config/values.env; . /defaults/defaults.env;
       exec eth2-val-tools keystores --insecure --source-mnemonic "$EL_AND_CL_MNEMONIC" \
         --source-min 0 --source-max "$NUMBER_OF_VALIDATORS" --out-loc /data/keys' \
     >>"$log" 2>&1; then
  echo "l1-init: keystore generation failed, last lines:"
  tail -20 "$log"
  remove_state
  exit 1
fi

el_ts="$(jq -r '.timestamp' "$dir/genesis/metadata/genesis.json")"
if [ "$(printf '%d' "$el_ts")" -lt "$genesis_timestamp" ]; then
  echo "l1-init: genesis.json timestamp $el_ts is before $genesis_timestamp; generator ignored the genesis time"
  remove_state
  exit 1
fi

echo "l1-init: genesis at $(date -r "$(printf '%d' "$el_ts")" '+%H:%M:%S'), $(find "$dir/genesis/keys/keys" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ') validator keystores created"
