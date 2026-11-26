#!/usr/bin/env bash
# Withdraws ETH from the L2 to the L1, the whole way, and checks every wei. The work is
# done by cmd/l2-withdraw; this script only hands it the devnet mnemonic, without
# printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l2-withdraw "$@"
