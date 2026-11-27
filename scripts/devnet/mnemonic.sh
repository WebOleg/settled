#!/usr/bin/env bash
# Prints the devnet mnemonic to stdout for a calling script to capture. It is the public
# test mnemonic of ethereum-genesis-generator, read the same way, from the same sources
# and in the same order, as the genesis that funded its accounts.
# Callers pass it on through the environment and never print it.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
gen_image="ethpandaops/ethereum-genesis-generator:6.2.9@sha256:90c046fbc50da48b0a642e48f00764da59275c889b34e338cbcea242f1d7f6fd"
values="$root/infra/devnet/l1/values.env"

# The script runs in the container and its variables expand there, so it goes in as a
# quoted here-document, which the host shell does not expand.
mnemonic="$(docker run --rm -i --entrypoint sh -v "$values:/config/values.env:ro" "$gen_image" -s <<'SH'
. /config/values.env
. /defaults/defaults.env
printf %s "$EL_AND_CL_MNEMONIC"
SH
)"
if [ -z "$mnemonic" ]; then
  echo "mnemonic: could not read the devnet mnemonic" >&2
  exit 1
fi
printf '%s' "$mnemonic"
