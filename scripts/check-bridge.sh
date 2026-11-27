#!/usr/bin/env bash
# The deposit indexer end to end against a real node that can be made to reorganise
# (cmd/bridge-e2e): anvil from the pinned Foundry image and a throwaway PostgreSQL,
# both on random local ports, removed when the run ends. The schema is migrated and the
# indexer's login made by ledger-migrate, as on the devnet; passwords live in files
# readable by the owner only and are never printed.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/postgres.sh
. scripts/lib/postgres.sh
: "${FOUNDRY_IMAGE:?FOUNDRY_IMAGE is not set; run make check-bridge}"

scripts/contracts/build.sh
pg="settled-check-bridge-pg-$$"
anvil="settled-check-bridge-anvil-$$"
dir="$(mktemp -d)"
cleanup() {
  docker rm -f "$pg" "$anvil" >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT
chmod 700 "$dir"

docker run -d --name "$anvil" -p 127.0.0.1::8545 --entrypoint anvil "$FOUNDRY_IMAGE" --host 0.0.0.0 --silent >/dev/null
start_postgres "$pg"
anvil_port="$(docker port "$anvil" 8545/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 1 30); do
  curl -sf -m 2 -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' \
    "http://127.0.0.1:$anvil_port" >/dev/null && break
  sleep 1
done

(umask 077 && printf '%s\n' "$PG_PASSWORD" > "$dir/admin" && openssl rand -hex 24 > "$dir/indexer")
unset PG_PASSWORD
go run ./cmd/ledger-migrate -db "postgres://admin@127.0.0.1:${PG_PORT}/ledger?sslmode=disable" -db-password-file "$dir/admin" \
  -login "indexer:deposit_indexer:$dir/indexer"
go run ./cmd/bridge-e2e -rpc "http://127.0.0.1:$anvil_port" -out contracts/out \
  -db "postgres://indexer@127.0.0.1:${PG_PORT}/ledger?sslmode=disable" -db-password-file "$dir/indexer"
echo "check-bridge: ok"
