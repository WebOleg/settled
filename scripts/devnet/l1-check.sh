#!/usr/bin/env bash
# Shows that the local L1 works: the execution and consensus layers answer, both advance,
# they agree on the head, and the beacon head is at the current slot. Prints the finality status.
# Exits 1 on the first failed check. Waits up to 180 s for both layers to answer and for
# the first block after genesis.
set -euo pipefail

EL="http://127.0.0.1:8545"
CL="http://127.0.0.1:4000"
EXPECTED_CHAIN_ID="0x301824" # 3151908

rpc() {
  curl -sf -m 3 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}" "$EL"
}
beacon() { curl -sf -m 3 "$CL$1"; }
fail() { echo "l1-check: FAIL: $*"; exit 1; }
el_block() { printf '%d' "$(rpc eth_blockNumber '[]' | jq -r '.result')"; }
cl_slot() { beacon /eth/v1/beacon/headers/head | jq -r '.data.header.message.slot'; }

# A fresh beacon node opens its HTTP API only after it has loaded the genesis state,
# and the first block arrives only after genesis time. Both take a while.
waited=0
until rpc eth_chainId '[]' >/dev/null 2>&1 && beacon /eth/v1/node/version >/dev/null 2>&1; do
  [ "$waited" -lt 180 ] || break
  sleep 5; waited=$((waited + 5))
done

chain_id="$(rpc eth_chainId '[]' | jq -r '.result')" || fail "execution client does not answer on $EL"
[ "$chain_id" = "$EXPECTED_CHAIN_ID" ] || fail "chain id $chain_id, expected $EXPECTED_CHAIN_ID"
echo "execution        answers, chain id 3151908"

beacon /eth/v1/node/version >/dev/null 2>&1 || fail "beacon node does not answer on $CL after 180 s"
echo "beacon           answers, $(beacon /eth/v1/node/version | jq -r '.data.version' | cut -d/ -f1-2)"

until [ "$(el_block)" -gt 0 ]; do
  [ "$waited" -lt 180 ] || fail "no block after 180 s"
  sleep 5; waited=$((waited + 5))
done
echo "first block      after ${waited} s"

b1="$(el_block)"; s1="$(cl_slot)"
sleep 13
b2="$(el_block)"; s2="$(cl_slot)"
[ "$b2" -gt "$b1" ] || fail "execution head did not advance ($b1 -> $b2)"
[ "$s2" -gt "$s1" ] || fail "beacon head did not advance ($s1 -> $s2)"
echo "progress         blocks $b1 -> $b2, slots $s1 -> $s2 in 13 s"

# Synced means the head is at, or one slot behind, the current slot. The is_syncing
# flag alone is not used: it is printed so its value on a node without peers is visible.
syncing="$(beacon /eth/v1/node/syncing)"
distance="$(printf '%s' "$syncing" | jq -r '.data.sync_distance')"
is_syncing="$(printf '%s' "$syncing" | jq -r '.data.is_syncing')"
[ "$distance" -le 1 ] || fail "beacon head is $distance slots behind the current slot"
[ "$(printf '%s' "$syncing" | jq -r '.data.el_offline')" = "false" ] || fail "beacon node reports the execution client offline"
echo "sync             head $distance slot(s) behind current, execution client online (is_syncing=$is_syncing)"

cl_exec="$(beacon /eth/v2/beacon/blocks/head | jq -r '.data.message.body.execution_payload.block_number')"
el_now="$(el_block)"
diff=$(( el_now - cl_exec ))
[ "$diff" -ge 0 ] && [ "$diff" -le 1 ] || fail "beacon head payload block $cl_exec, execution head $el_now"
echo "agreement        beacon head carries execution block $cl_exec, execution head $el_now"

fin="$(beacon /eth/v1/beacon/states/head/finality_checkpoints | jq -r '.data.finalized.epoch')"
tag_block() {
  local n
  n="$(rpc eth_getBlockByNumber "[\"$1\", false]" | jq -r '.result.number // empty')"
  if [ -n "$n" ]; then printf '%d' "$n"; else printf 'none yet'; fi
}
safe="$(tag_block safe)"
final="$(tag_block finalized)"
echo "finality         finalized epoch $fin, safe block $safe, finalized block $final"
echo "l1-check: ok"
