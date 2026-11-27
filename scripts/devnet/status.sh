#!/usr/bin/env bash
# One look at the whole devnet, without waiting: which services run, the L1 heads, the
# L2 heads on the sequencer and the verifier, the scrape targets and firing alerts, the
# number of dispute games, and the L1 balances of the accounts that pay for batches and
# proposals. Exits 1 if a service is
# down or does not answer.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

L1_EL="http://127.0.0.1:8545"
L1_CL="http://127.0.0.1:4000"
SEQ_NODE="http://127.0.0.1:19545"
VER_NODE="http://127.0.0.1:29545"
L1="docker compose -f infra/devnet/l1/compose.yaml"
L2="docker compose -f infra/devnet/l2/compose.yaml"
BRIDGE="docker compose -f infra/bridge/compose.yaml"

problems=0
call() {
  curl -sf -m 3 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "$1" | jq -r '.result'
}
num() { if [ -n "$1" ] && [ "$1" != "null" ]; then printf '%d' "$1"; else echo "?"; fi; }

echo "Services"
for pair in "l1:geth" "l1:beacon" "l1:validator" "l2:op-reth" "l2:op-node" "l2:op-batcher" \
  "l2:verifier-op-reth" "l2:verifier-op-node" "l2:op-proposer" "bridge:postgres" "bridge:deposit-indexer"; do
  layer="${pair%%:*}"; svc="${pair#*:}"
  case "$layer" in l1) compose="$L1" ;; l2) compose="$L2" ;; *) compose="$BRIDGE" ;; esac
  state="$($compose ps -a --format '{{.State}}' "$svc" 2>/dev/null | head -1)"
  [ -n "$state" ] || state="not created"
  printf '  %-6s %-20s %s\n' "$layer" "$svc" "$state"
  [ "$state" = "running" ] || problems=$((problems + 1))
done

echo
echo "L1 (6 s slots)"
if head="$(call "$L1_EL" eth_blockNumber '[]')" && [ -n "$head" ] && [ "$head" != "null" ]; then
  safe="$(call "$L1_EL" eth_getBlockByNumber '["safe", false]' | jq -r '.number' 2>/dev/null || echo "")"
  final="$(call "$L1_EL" eth_getBlockByNumber '["finalized", false]' | jq -r '.number' 2>/dev/null || echo "")"
  printf '  %-12s head %s, safe %s, finalized %s\n' "geth" "$(num "$head")" "$(num "$safe")" "$(num "$final")"
  slot="$(curl -sf -m 3 "$L1_CL/eth/v1/node/syncing" | jq -r '.data.head_slot' 2>/dev/null || echo "?")"
  printf '  %-12s head slot %s\n' "beacon" "$slot"
else
  echo "  geth does not answer"; problems=$((problems + 1))
fi

echo
echo "L2 (2 s blocks)"
for pair in "sequencer:$SEQ_NODE" "verifier:$VER_NODE"; do
  name="${pair%%:*}"; url="${pair#*:}"
  if s="$(call "$url" optimism_syncStatus '[]')" && [ -n "$s" ] && [ "$s" != "null" ]; then
    printf '  %-12s unsafe %s, safe %s, finalized %s; L1 origin %s\n' "$name" \
      "$(printf '%s' "$s" | jq -r '.unsafe_l2.number')" "$(printf '%s' "$s" | jq -r '.safe_l2.number')" \
      "$(printf '%s' "$s" | jq -r '.finalized_l2.number')" "$(printf '%s' "$s" | jq -r '.unsafe_l2.l1origin.number')"
  else
    printf '  %-12s op-node does not answer\n' "$name"; problems=$((problems + 1))
  fi
done

echo
echo "Observability"
prom="$(curl -sf -m 3 'http://127.0.0.1:9090/api/v1/targets?state=active' 2>/dev/null || true)"
if [ -n "$prom" ]; then
  printf '  %-12s %s of %s targets up (http://127.0.0.1:9090)\n' "prometheus" \
    "$(printf '%s' "$prom" | jq '[.data.activeTargets[] | select(.health == "up")] | length')" \
    "$(printf '%s' "$prom" | jq '.data.activeTargets | length')"
  firing="$(curl -sf -m 3 'http://127.0.0.1:9090/api/v1/alerts' 2>/dev/null \
    | jq -r '[.data.alerts[] | select(.state == "firing") | .labels.alertname] | unique | join(", ")' 2>/dev/null || echo "?")"
  printf '  %-12s %s (http://127.0.0.1:9093)\n' "alerts" "${firing:-none firing}"
else
  echo "  prometheus   not running (make obs-up)"
fi

echo
echo "Dispute games"
dgf="$(jq -r '.DisputeGameFactoryProxy // empty' .devnet/l2/deployer/l1-addresses.json 2>/dev/null || true)"
if [ -n "$dgf" ]; then
  count="$(call "$L1_EL" eth_call "[{\"to\":\"$dgf\",\"data\":\"0x4d1975b4\"},\"latest\"]" 2>/dev/null || echo "")"
  printf '  %-12s %s games (make l2-games checks them)\n' "factory" "$(num "$count")"
else
  echo "  no deployment yet"
fi

echo
echo "L1 balances of the operator accounts"
if [ -f .devnet/l2/keys/addresses.json ]; then
  for role in batcher proposer challenger; do
    addr="$(jq -r --arg r "$role" '.[] | select(.role == $r) | .address' .devnet/l2/keys/addresses.json)"
    wei="$(call "$L1_EL" eth_getBalance "[\"$addr\", \"latest\"]" 2>/dev/null || echo "")"
    if [ -n "$wei" ] && [ "$wei" != "null" ]; then
      eth="$(echo "scale=4; $(echo "ibase=16; $(printf '%s' "${wei#0x}" | tr 'a-f' 'A-F')" | bc) / 10^18" | bc)"
      printf '  %-12s %s ETH\n' "$role" "$eth"
    else
      printf '  %-12s unknown\n' "$role"
    fi
  done
else
  echo "  no role keys yet (make l2-keys)"
fi

echo
if [ "$problems" -eq 0 ]; then echo "status: ok"; else echo "status: $problems problem(s)"; exit 1; fi
