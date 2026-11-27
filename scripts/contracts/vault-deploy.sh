#!/usr/bin/env bash
# Deploys the vault to the devnet L2 (cmd/vault-deploy) after building the contracts, so
# the code deployed is the code in the repository. Hands the devnet mnemonic over
# without printing it. With --reset, deletes the deployment record and keeps the keys,
# for a new L2.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
if [ "${1:-}" = "--reset" ]; then
  rm -f .devnet/vault/record/deployment.json
  echo "vault-deploy: removed .devnet/vault/record/deployment.json; keys kept"
  exit 0
fi
scripts/contracts/build.sh
mnemonic="$(scripts/devnet/mnemonic.sh)"
SETTLED_DEVNET_MNEMONIC="$mnemonic" exec go run ./cmd/vault-deploy "$@"
