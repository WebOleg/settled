#!/usr/bin/env bash
# Deploys the devnet L2 contracts to the local L1 with op-deployer v0.9.0, pinned by
# digest, then exports genesis.json, rollup.json and the L1 contract addresses into
# .devnet/l2/deployer, and records the hash of the L1 genesis block it was deployed on.
# Runs once: a finished deployment on the running L1 is left alone. A deployment made on
# an L1 that has since been reset is refused; make reset starts over. With --reset,
# deletes the deployment.
#
# --no-verify: there is no block explorer on the devnet to publish contract sources to;
# without it op-deployer logs a successful deployment with error lines.
#
# The deployer key reaches the container through the environment only (-e NAME with no
# value), so it never appears on a command line or in the process list.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

IMG="us-docker.pkg.dev/oplabs-tools-artifacts/images/op-deployer:v0.9.0@sha256:887ddaae8130c0510c6d4c8bc523d09f0d33c40696bee0e80c6a3dc243a0e7c8"
L1_NET="settled-l1_default"
L1_CHAIN_ID=3151908
L2_CHAIN_ID=420420
keys=".devnet/l2/keys"
work=".devnet/l2/deployer"

remove_work() {
  case "$PWD/$work" in
    */.devnet/l2/deployer) rm -rf "$work" ;;
    *) echo "l2-deploy: refusing to delete $work"; exit 1 ;;
  esac
}

if [ "${1:-}" = "--reset" ]; then
  remove_work
  echo "l2-deploy: removed $work (role keys kept)"
  exit 0
fi

l1_call() {
  curl -sf -m 5 -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}" http://127.0.0.1:8545 | jq -r '.result'
}
l1_genesis_hash() { l1_call eth_getBlockByNumber '["0x0", false]' | jq -r '.hash'; }

if [ -f "$work/state.json" ] && jq -e '(.opChainDeployments // []) | length > 0' "$work/state.json" >/dev/null 2>&1; then
  live="$(l1_genesis_hash)" || { echo "l2-deploy: the L1 does not answer on 127.0.0.1:8545"; exit 1; }
  if [ ! -f "$work/l1-genesis-hash" ]; then
    # A deployment made before the hash was recorded: accept it only if its SystemConfig
    # has code on the running L1, then record the hash.
    sc="$(jq -r '.SystemConfigProxy' "$work/l1-addresses.json")"
    code="$(l1_call eth_getCode "[\"$sc\", \"latest\"]" || true)"
    [ -n "$code" ] && [ "$code" != "0x" ] && [ "$code" != "null" ] \
      || { echo "l2-deploy: the deployment's SystemConfig $sc has no code on this L1; run make reset"; exit 1; }
    printf '%s\n' "$live" > "$work/l1-genesis-hash"
    echo "l2-deploy: recorded the L1 genesis hash of the existing deployment"
  fi
  [ "$(cat "$work/l1-genesis-hash")" = "$live" ] \
    || { echo "l2-deploy: the deployment was made on another L1 (genesis hash differs); run make reset"; exit 1; }
  echo "l2-deploy: already deployed on this L1 (see $work/state.json), nothing to do"
  exit 0
fi
[ -f "$keys/addresses.json" ] || { echo "l2-deploy: no role keys; run make l2-keys and make l2-fund first"; exit 1; }
docker network inspect "$L1_NET" >/dev/null 2>&1 || { echo "l2-deploy: the L1 is not running; run make l1-up"; exit 1; }

# A partial deployment directory is never reused.
remove_work
mkdir -p "$work"
chmod 700 "$work"

# run_opd [docker options] -- [op-deployer arguments]. The empty-array form keeps
# bash 3.2 (macOS) from failing under set -u when there are no docker options.
run_opd() {
  local opts=()
  while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do opts+=("$1"); shift; done
  shift
  docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp --network "$L1_NET" \
    -v "$PWD/$work:/work" ${opts[@]+"${opts[@]}"} --entrypoint op-deployer "$IMG" "$@"
}

version="$(docker run --rm --entrypoint op-deployer "$IMG" --version | awk '{print $3}')"
echo "l2-deploy: op-deployer $version"

run_opd -- init --l1-chain-id "$L1_CHAIN_ID" --l2-chain-ids "$L2_CHAIN_ID" \
  --workdir /work --intent-type custom >"$work/init.log" 2>&1 \
  || { tail -20 "$work/init.log"; exit 1; }

go run ./cmd/l2-intent --addresses "$keys/addresses.json" --op-deployer-version "$version" \
  --l1-chain-id "$L1_CHAIN_ID" --l2-chain-id "$L2_CHAIN_ID" --out "$work/intent.toml"

echo "l2-deploy: applying to the L1 (this sends many transactions; it takes a few minutes)"
started=$(date +%s)
if ! (
  DEPLOYER_PRIVATE_KEY="0x$(tr -d '[:space:]' < "$keys/deployer.key")"
  export DEPLOYER_PRIVATE_KEY
  run_opd -e DEPLOYER_PRIVATE_KEY -- apply --workdir /work \
    --l1-rpc-url http://geth:8545 --deployment-target live --no-verify
) >"$work/apply.log" 2>&1; then
  echo "l2-deploy: apply failed, last lines:"
  tail -30 "$work/apply.log"
  exit 1
fi
echo "l2-deploy: applied in $(( $(date +%s) - started )) s"

for what in genesis rollup l1; do
  if ! run_opd -- inspect "$what" --workdir /work "$L2_CHAIN_ID" >"$work/$what.json" 2>"$work/inspect-$what.log"; then
    echo "l2-deploy: inspect $what failed:"; tail -10 "$work/inspect-$what.log"; exit 1
  fi
done
mv "$work/l1.json" "$work/l1-addresses.json"
l1_genesis_hash > "$work/l1-genesis-hash"
echo "l2-deploy: wrote genesis.json, rollup.json, l1-addresses.json, l1-genesis-hash to $work"
