#!/usr/bin/env bash
# Deposits ETH from the L1 to the L2 and checks every wei of it. The work is done by
# cmd/l2-deposit; this script only hands it the devnet mnemonic, without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l2-deposit "$@"
