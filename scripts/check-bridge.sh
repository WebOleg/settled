#!/usr/bin/env bash
# The deposit indexer end to end against a real node that can be made to reorganise
# (cmd/bridge-e2e): anvil from the pinned Foundry image and a throwaway PostgreSQL,
# both on random local ports, removed when the run ends. The schema is migrated and the
# indexer's login made by ledger-migrate, as on the devnet; passwords live in files
# readable by the owner only and are never printed. Then the payment the e2e made
# without the ledger is resolved with cmd/suspense by two operators, each with a login
# of their own: the one who proposed cannot approve it, the second can. Last, the
# withdrawal payer end to end (cmd/payer-e2e) on an anvil and a database of its own:
# crashes, a reorganisation, two payers at once, a fee rise and the vault's queue.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/postgres.sh
. scripts/lib/postgres.sh
: "${FOUNDRY_IMAGE:?FOUNDRY_IMAGE is not set; run make check-bridge}"

scripts/contracts/build.sh
pg="settled-check-bridge-pg-$$"
anvil="settled-check-bridge-anvil-$$"
anvil2="settled-check-bridge-anvil2-$$"
dir="$(mktemp -d)"
cleanup() {
  docker rm -f "$pg" "$anvil" "$anvil2" >/dev/null 2>&1 || true
  rm -rf "$dir"
}
trap cleanup EXIT
chmod 700 "$dir"

for a in "$anvil" "$anvil2"; do
  docker run -d --name "$a" -p 127.0.0.1::8545 --entrypoint anvil "$FOUNDRY_IMAGE" --host 0.0.0.0 --silent >/dev/null
done
start_postgres "$pg"
anvil_port="$(docker port "$anvil" 8545/tcp | head -1 | sed 's/.*://')"
anvil2_port="$(docker port "$anvil2" 8545/tcp | head -1 | sed 's/.*://')"
for port in "$anvil_port" "$anvil2_port"; do
  for _ in $(seq 1 30); do
    curl -sf -m 2 -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' \
      "http://127.0.0.1:$port" >/dev/null && break
    sleep 1
  done
done

(umask 077 && printf '%s\n' "$PG_PASSWORD" > "$dir/admin" && for f in indexer ops_a ops_b requester payer; do openssl rand -hex 24 > "$dir/$f"; done)
unset PG_PASSWORD
go run ./cmd/ledger-migrate -db "postgres://admin@127.0.0.1:${PG_PORT}/ledger?sslmode=disable" -db-password-file "$dir/admin" \
  -login "indexer:deposit_indexer:$dir/indexer" -login "ops_a:suspense_operator:$dir/ops_a" -login "ops_b:suspense_operator:$dir/ops_b"
go run ./cmd/bridge-e2e -rpc "http://127.0.0.1:$anvil_port" -out contracts/out \
  -db "postgres://indexer@127.0.0.1:${PG_PORT}/ledger?sslmode=disable" -db-password-file "$dir/indexer"

go build -o "$dir/suspense" ./cmd/suspense
as() { "$dir/suspense" -db "postgres://$1@127.0.0.1:${PG_PORT}/ledger?sslmode=disable" -db-password-file "$dir/$1" "${@:2}"; }
fail() { echo "check-bridge: FAIL: $*"; exit 1; }
listed="$(as ops_a list)"
event="$(printf '%s\n' "$listed" | awk '$1 == "event" { print $2; exit }')"
if [ -z "$event" ] || ! printf '%s\n' "$listed" | grep -q '^suspense: 1 payments'; then fail "want one payment in suspense: $listed"; fi
proposed="$(as ops_a propose -event "$event" -writeoff -reason "the e2e paid it without the ledger")"
id="$(printf '%s\n' "$proposed" | awk '$1 == "proposal" { sub(":", "", $2); print $2 }')"
printf 'ok    %-50s %s\n' "proposed by ops_a" "$proposed"
if out="$(as ops_a approve -id "$id" -note "my own" 2>&1)"; then fail "ops_a approved their own proposal: $out"; fi
printf '%s\n' "$out" | grep -q "a second operator approves" || fail "the proposer's approval failed otherwise: $out"
printf 'ok    %-50s %s\n' "ops_a cannot approve it" "refused"
printf 'ok    %-50s %s\n' "approved by ops_b" "$(as ops_b approve -id "$id" -note "matches the e2e's payment")"
as ops_b list | grep -q '^suspense: 0 payments' || fail "the payment is still in suspense after approval"
printf 'ok    %-50s %s\n' "suspense is explained" "0 payments"

docker exec "$pg" psql -q -U admin -d ledger -c "CREATE DATABASE payments" >/dev/null
go run ./cmd/ledger-migrate -db "postgres://admin@127.0.0.1:${PG_PORT}/payments?sslmode=disable" -db-password-file "$dir/admin" \
  -login "indexer:deposit_indexer:$dir/indexer" -login "requester:withdrawal_requester:$dir/requester" -login "payer:withdrawal_payer:$dir/payer"
db() { printf -- '-%s-db\npostgres://%s@127.0.0.1:%s/payments?sslmode=disable\n-%s-db-password-file\n%s\n' "$1" "$1" "$PG_PORT" "$1" "$dir/$1"; }
args=()
while IFS= read -r a; do args+=("$a"); done < <(db requester; db payer; db indexer)
go run ./cmd/payer-e2e -rpc "http://127.0.0.1:$anvil2_port" -out contracts/out "${args[@]}"
echo "check-bridge: ok"
