#!/usr/bin/env bash
# Prepares the runtime folders of the L2 sequencer, verifier and proposer: one engine
# API secret per op-node and op-reth pair and one peer-to-peer node key per op-node, each
# created once with mode 600, the L1 chain config for op-node, the verifier's static
# peer address and the proposer's game factory address. Checks that the role keys
# mounted into containers exist and only their owner can read them. With --reset,
# deletes all of that; the role keys and the contract deployment are kept.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
seq="$root/.devnet/l2/sequencer"
ver="$root/.devnet/l2/verifier"
prop="$root/.devnet/l2/proposer"
deployer="$root/.devnet/l2/deployer"
keys="$root/.devnet/l2/keys"

remove_state() {
  for d in "$seq" "$ver" "$prop"; do
    case "$d" in
      */.devnet/l2/sequencer | */.devnet/l2/verifier | */.devnet/l2/proposer) rm -rf "$d" ;;
      *) echo "l2-init: refusing to delete $d"; exit 1 ;;
    esac
  done
}

if [ "${1:-}" = "--reset" ]; then
  remove_state
  echo "l2-init: removed .devnet/l2/sequencer, verifier and proposer (keys and deployment kept)"
  exit 0
fi

for f in genesis.json rollup.json l1-genesis-hash; do
  [ -f "$deployer/$f" ] || { echo "l2-init: no $f; run make l2-deploy first"; exit 1; }
done
# The L2 belongs to the L1 it was deployed on. After an L1 reset its chain config and
# data are meaningless, so refuse to start.
live="$(curl -sf -m 5 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x0",false]}' \
  http://127.0.0.1:8545 | jq -r '.result.hash')" || { echo "l2-init: the L1 does not answer; run make l1-up"; exit 1; }
[ "$(cat "$deployer/l1-genesis-hash")" = "$live" ] \
  || { echo "l2-init: the L2 was deployed on another L1 (genesis hash differs); run make reset"; exit 1; }

mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }
want_mode() {
  local perm
  perm="$(mode "$1")"
  [ "$perm" = "$2" ] || { echo "l2-init: $1 has mode $perm, want $2"; exit 1; }
}
# A secret that is created once and never replaced: 32 random bytes as hex, mode 600.
secret_once() {
  if [ ! -f "$1" ]; then
    (umask 077 && openssl rand -hex 32 > "$1")
    echo "l2-init: created $2"
  fi
  want_mode "$1" 600
}

for d in "$seq" "$ver"; do
  mkdir -p "$d/op-reth" "$d/op-node"
  chmod 700 "$d"
done

# op-node needs the L1 chain config: the `config` object of the L1 genesis, not the whole
# file. op-node decodes it strictly, and the genesis generator still writes fields that
# go-ethereum has removed (for example terminalTotalDifficultyPassed). Only the fields of
# go-ethereum's ChainConfig at commit 3dee18e7108c, the version op-node v1.21.0 is built
# on, are kept; anything dropped is printed. Rewritten on every run so it always matches
# the running L1. Recheck this list when op-node is upgraded. Both op-nodes read it.
l1_genesis="$root/.devnet/l1/genesis/metadata/genesis.json"
[ -f "$l1_genesis" ] || { echo "l2-init: no L1 genesis; run make l1-up first"; exit 1; }
known='["chainId","homesteadBlock","daoForkBlock","daoForkSupport","eip150Block","eip155Block",
  "eip158Block","byzantiumBlock","constantinopleBlock","petersburgBlock","istanbulBlock",
  "muirGlacierBlock","berlinBlock","londonBlock","arrowGlacierBlock","grayGlacierBlock",
  "mergeNetsplitBlock","shanghaiTime","cancunTime","pragueTime","osakaTime","bpo1Time",
  "bpo2Time","bpo3Time","bpo4Time","bpo5Time","amsterdamTime","bogotaTime","ubtTime",
  "terminalTotalDifficulty","depositContractAddress","enableUBTAtGenesis","ethash","clique",
  "blobSchedule"]'
jq -e '.config' "$l1_genesis" >/dev/null || { echo "l2-init: $l1_genesis has no config object"; exit 1; }
dropped="$(jq -r --argjson known "$known" '.config | keys - $known | join(" ")' "$l1_genesis")"
jq --argjson known "$known" '.config | with_entries(select(.key as $k | $known | index($k)))' \
  "$l1_genesis" > "$seq/l1-chain-config.json"
if [ -n "$dropped" ]; then
  echo "l2-init: L1 chain config for op-node; dropped fields op-node does not know: $dropped"
fi

# One engine API secret per pair: a secret shared across pairs would let one node's
# credentials drive the other engine.
secret_once "$seq/jwt.hex" "the sequencer engine API secret"
secret_once "$ver/jwt.hex" "the verifier engine API secret"

# One peer-to-peer node key per op-node. Fixed keys give fixed peer IDs, so the verifier
# can be given the sequencer's full address before either node starts.
secret_once "$seq/p2p.key" "the sequencer peer-to-peer key"
secret_once "$ver/p2p.key" "the verifier peer-to-peer key"
seq_peer="$(cd "$root" && go run ./cmd/l2-peer-id "$seq/p2p.key")"
ver_peer="$(cd "$root" && go run ./cmd/l2-peer-id "$ver/p2p.key")"
printf 'OP_NODE_P2P_STATIC=/dns4/op-node/tcp/9222/p2p/%s\n' "$seq_peer" > "$ver/p2p.env"
printf '%s\n' "$seq_peer" > "$seq/peer-id"
printf '%s\n' "$ver_peer" > "$ver/peer-id"

# The proposer creates games on this deployment's DisputeGameFactory. Rewritten on every
# run, so it always matches the deployment.
mkdir -p "$prop"
chmod 700 "$prop"
dgf="$(jq -r '.DisputeGameFactoryProxy // empty' "$deployer/l1-addresses.json")"
[ -n "$dgf" ] || { echo "l2-init: no DisputeGameFactoryProxy in l1-addresses.json"; exit 1; }
printf 'OP_PROPOSER_GAME_FACTORY_ADDRESS=%s\n' "$dgf" > "$prop/proposer.env"

# Role keys mounted as secret files: the batcher key into op-batcher, the sequencer key
# into the sequencer op-node, the proposer key into op-proposer. Refuse a missing key or
# one that others can read, the same rule make l2-keys applies; never fix the mode
# silently.
want_mode "$keys" 700
for role in batcher sequencer proposer; do
  [ -f "$keys/$role.key" ] || { echo "l2-init: no $role key; run make l2-keys first"; exit 1; }
  want_mode "$keys/$role.key" 600
done
echo "l2-init: ready; sequencer peer $seq_peer, verifier peer $ver_peer"
