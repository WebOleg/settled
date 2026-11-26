#!/usr/bin/env bash
# Shows that the L2 sequencer works: op-reth answers with the L2 chain id, the unsafe
# head advances at the 2 s block time, op-node follows the L1, and op-reth and op-node
# agree on the head. Prints the safe and finalized heads, which stay at genesis until a
# batcher posts batches. Waits up to 120 s for the first L2 block.
set -euo pipefail

EL="http://127.0.0.1:18545"
NODE="http://127.0.0.1:19545"
EXPECTED_CHAIN_ID="0x66a44" # 420420

call() {
  curl -sf -m 3 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "$1"
}
fail() { echo "l2-check: FAIL: $*"; exit 1; }
status() { call "$NODE" optimism_syncStatus '[]' | jq -c '.result'; }
el_block() { printf '%d' "$(call "$EL" eth_blockNumber '[]' | jq -r '.result')"; }

waited=0
until call "$EL" eth_chainId '[]' >/dev/null 2>&1 && call "$NODE" optimism_syncStatus '[]' >/dev/null 2>&1; do
  [ "$waited" -lt 120 ] || fail "op-reth or op-node does not answer after 120 s"
  sleep 5; waited=$((waited + 5))
done

chain_id="$(call "$EL" eth_chainId '[]' | jq -r '.result')"
[ "$chain_id" = "$EXPECTED_CHAIN_ID" ] || fail "chain id $chain_id, expected $EXPECTED_CHAIN_ID"
echo "op-reth          answers, chain id 420420"

until [ "$(el_block)" -gt 0 ]; do
  [ "$waited" -lt 120 ] || fail "no L2 block after 120 s"
  sleep 5; waited=$((waited + 5))
done

s1="$(status)"; b1="$(el_block)"
sleep 10
s2="$(status)"; b2="$(el_block)"
u1="$(printf '%s' "$s1" | jq -r '.unsafe_l2.number')"
u2="$(printf '%s' "$s2" | jq -r '.unsafe_l2.number')"
[ "$b2" -gt "$b1" ] || fail "op-reth head did not advance ($b1 -> $b2)"
[ "$u2" -gt "$u1" ] || fail "unsafe head did not advance ($u1 -> $u2)"
echo "progress         L2 blocks $b1 -> $b2 in 10 s (block time 2 s)"

o1="$(printf '%s' "$s1" | jq -r '.unsafe_l2.l1origin.number')"
o2="$(printf '%s' "$s2" | jq -r '.unsafe_l2.l1origin.number')"
head_l1="$(printf '%s' "$s2" | jq -r '.head_l1.number')"
[ "$o2" -ge "$o1" ] && [ "$head_l1" -gt 0 ] || fail "op-node does not follow the L1 (origin $o1 -> $o2, head_l1 $head_l1)"
echo "l1               op-node sees L1 head $head_l1; L2 head's L1 origin $o2"

el_hash="$(call "$EL" eth_getBlockByNumber "[\"0x$(printf '%x' "$u2")\", false]" | jq -r '.result.hash')"
node_hash="$(printf '%s' "$s2" | jq -r '.unsafe_l2.hash')"
[ "$el_hash" = "$node_hash" ] || fail "block $u2: op-reth $el_hash, op-node $node_hash"
echo "agreement        block $u2 has the same hash in op-reth and op-node"

safe="$(printf '%s' "$s2" | jq -r '.safe_l2.number')"
final="$(printf '%s' "$s2" | jq -r '.finalized_l2.number')"
echo "safety           unsafe $u2, safe $safe, finalized $final (safe moves once batches reach the L1)"
echo "l2-check: ok"
