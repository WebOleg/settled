#!/usr/bin/env bash
# Sends one blob transaction on the local L1 from the first funded devnet account and
# waits until it is included with blob gas used. The work is done by cmd/l1-blob-check;
# this script only hands it the devnet mnemonic, without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l1-blob-check "$@"
