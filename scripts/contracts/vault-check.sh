#!/usr/bin/env bash
# Runs the vault's rules on the devnet L2 with real transactions (cmd/vault-check) after
# building the contracts, so the ABI is the one of the code in the repository. Hands the
# devnet mnemonic over without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
scripts/contracts/build.sh
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/vault-check "$@"
