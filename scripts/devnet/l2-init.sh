#!/usr/bin/env bash
# Prepares the runtime folders of the L2 sequencer and its engine API secret, once.
# With --reset, deletes the sequencer's chain data and secret; the role keys and the
# contract deployment are kept.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
dir="$root/.devnet/l2/sequencer"
deployer="$root/.devnet/l2/deployer"

remove_state() {
  case "$dir" in
    */.devnet/l2/sequencer) rm -rf "$dir" ;;
    *) echo "l2-init: refusing to delete $dir"; exit 1 ;;
  esac
}

if [ "${1:-}" = "--reset" ]; then
  remove_state
  echo "l2-init: removed .devnet/l2/sequencer (keys and deployment kept)"
  exit 0
fi

for f in genesis.json rollup.json; do
  [ -f "$deployer/$f" ] || { echo "l2-init: no $f; run make l2-deploy first"; exit 1; }
done

mkdir -p "$dir/op-reth" "$dir/op-node"
chmod 700 "$dir"

# op-node needs the L1 chain config: the `config` object of the L1 genesis, not the whole
# file. op-node decodes it strictly, and the genesis generator still writes fields that
# go-ethereum has removed (for example terminalTotalDifficultyPassed). Only the fields of
# go-ethereum's ChainConfig at commit 3dee18e7108c, the version op-node v1.21.0 is built
# on, are kept; anything dropped is printed. Rewritten on every run so it always matches
# the running L1. Recheck this list when op-node is upgraded.
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
  "$l1_genesis" > "$dir/l1-chain-config.json"
if [ -n "$dropped" ]; then
  echo "l2-init: L1 chain config for op-node; dropped fields op-node does not know: $dropped"
fi

# One secret per op-node and op-reth pair, created once, readable by the owner only.
if [ ! -f "$dir/jwt.hex" ]; then
  (umask 077 && openssl rand -hex 32 > "$dir/jwt.hex")
  echo "l2-init: created the engine API secret"
fi
perm="$(stat -f '%Lp' "$dir/jwt.hex" 2>/dev/null || stat -c '%a' "$dir/jwt.hex")"
[ "$perm" = "600" ] || { echo "l2-init: $dir/jwt.hex has mode $perm, want 600"; exit 1; }
echo "l2-init: ready"
