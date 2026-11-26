#!/usr/bin/env bash
# Shows that the L2 works: op-reth answers with the L2 chain id, the unsafe head
# advances at the 2 s block time, op-node follows the L1, op-reth and op-node agree on
# the head, the safe head advances because the batcher posts batches, those batches are
# blob transactions to the batch inbox, and both clients agree on the safe block.
# Waits up to 120 s for the first L2 block and up to 4 min for the safe head.
set -euo pipefail

EL="http://127.0.0.1:18545"
NODE="http://127.0.0.1:19545"
L1="http://127.0.0.1:8545"
EXPECTED_CHAIN_ID="0x66a44" # 420420
LOOKBACK=50                 # L1 blocks searched for batches, 5 min at 6 s

root="$(git rev-parse --show-toplevel)"
batcher="$(jq -r '.[] | select(.role == "batcher") | .address' "$root/.devnet/l2/keys/addresses.json" | tr 'A-F' 'a-f')"
inbox="$(jq -r '.batch_inbox_address' "$root/.devnet/l2/deployer/rollup.json" | tr 'A-F' 'a-f')"

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

# Safe means derived from batches on the L1. The first batch needs one channel duration
# (30 s) plus inclusion; after that the safe head moves with every batch.
safe_num() { status | jq -r '.safe_l2.number'; }
t=0
until [ "$(safe_num)" -gt 0 ]; do
  [ "$t" -lt 240 ] || fail "safe head still at genesis after 240 s; no batch derived from the L1"
  sleep 5; t=$((t + 5))
done
safe1="$(safe_num)"; t=0
until [ "$(safe_num)" -gt "$safe1" ]; do
  [ "$t" -lt 120 ] || fail "safe head stuck at $safe1 for 120 s"
  sleep 5; t=$((t + 5))
done
s3="$(status)"
safe="$(printf '%s' "$s3" | jq -r '.safe_l2.number')"
final="$(printf '%s' "$s3" | jq -r '.finalized_l2.number')"
unsafe="$(printf '%s' "$s3" | jq -r '.unsafe_l2.number')"
[ "$safe" -le "$unsafe" ] || fail "safe $safe is ahead of unsafe $unsafe"
echo "safe             $safe1 -> $safe; unsafe $unsafe, finalized $final"

safe_hash="$(printf '%s' "$s3" | jq -r '.safe_l2.hash')"
el_safe_hash="$(call "$EL" eth_getBlockByNumber "[\"0x$(printf '%x' "$safe")\", false]" | jq -r '.result.hash')"
[ "$safe_hash" = "$el_safe_hash" ] || fail "safe block $safe: op-node $safe_hash, op-reth $el_safe_hash"
echo "safe agreement   block $safe has the same hash in op-reth and op-node"

# Every transaction from the batcher to the inbox in the last LOOKBACK L1 blocks: count
# them by type. Type 0x3 carries blobs; anything else means batches went as calldata.
l1_head="$(printf '%d' "$(call "$L1" eth_blockNumber '[]' | jq -r '.result')")"
from=$((l1_head > LOOKBACK ? l1_head - LOOKBACK : 0))
types="$(
  n="$from"
  while [ "$n" -le "$l1_head" ]; do
    call "$L1" eth_getBlockByNumber "[\"0x$(printf '%x' "$n")\", true]" |
      jq -r --arg from "$batcher" --arg to "$inbox" \
        '.result.transactions[] | select((.from | ascii_downcase) == $from and ((.to // "") | ascii_downcase) == $to) | .type'
    n=$((n + 1))
  done
)"
blob_txs="$(printf '%s\n' "$types" | grep -c '^0x3$' || true)"
other_txs=$(($(printf '%s\n' "$types" | grep -c . || true) - blob_txs))
[ "$blob_txs" -gt 0 ] || fail "no blob transaction from the batcher to the inbox in L1 blocks $from-$l1_head"
[ "$other_txs" -eq 0 ] || fail "$other_txs batcher transactions without blobs in L1 blocks $from-$l1_head"
echo "batches          $blob_txs blob transactions from the batcher to the inbox in L1 blocks $from-$l1_head, none as calldata"
echo "l2-check: ok"
