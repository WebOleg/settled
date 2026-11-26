#!/usr/bin/env bash
# Sends one blob transaction on the local L1 from the first funded devnet account and
# waits until it is included with blob gas used. The work is done by cmd/l1-blob-check;
# this script only hands it the devnet mnemonic, without printing it.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
gen_image="ethpandaops/ethereum-genesis-generator:6.2.9"
values="$root/infra/devnet/l1/values.env"

# The same sources, in the same order, as the genesis that funded the accounts.
# shellcheck disable=SC2016
mnemonic="$(docker run --rm --entrypoint sh -v "$values:/config/values.env:ro" "$gen_image" \
  -c '. /config/values.env; . /defaults/defaults.env; printf %s "$EL_AND_CL_MNEMONIC"')"
if [ -z "$mnemonic" ]; then
  echo "l1-blob-check: could not read the devnet mnemonic"
  exit 1
fi

SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l1-blob-check "$@"
