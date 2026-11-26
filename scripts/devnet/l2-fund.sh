#!/usr/bin/env bash
# Tops up the L2 operator accounts that pay gas on the L1. The work is done by
# cmd/l2-fund; this script only hands it the devnet mnemonic, without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/l2-fund "$@"
