#!/usr/bin/env bash
# Gives the withdrawal payer its key in kms-dev with the vault's operator role, through
# the timelock (cmd/vault-payer), after building the contracts, so the ABI is the one of
# the code in the repository; with -rotate, replaces the key. Hands the devnet mnemonic
# over without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
scripts/contracts/build.sh
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/vault-payer "$@"
