#!/usr/bin/env bash
# Moves the vault's admin to a new timelock with split roles (cmd/vault-timelock-move)
# after building the contracts, so the code deployed is the one in the repository. Hands
# the devnet mnemonic over without printing it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
scripts/contracts/build.sh
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/vault-timelock-move "$@"
