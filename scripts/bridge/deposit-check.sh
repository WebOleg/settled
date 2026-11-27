#!/usr/bin/env bash
# Sends one real deposit to the vault on the devnet and follows it through the ledger
# (cmd/deposit-check), after building the contracts, so the ABI is the one of the code
# in the repository. Hands the devnet mnemonic over without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
scripts/contracts/build.sh
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/deposit-check "$@"
