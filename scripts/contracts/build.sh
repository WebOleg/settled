#!/usr/bin/env bash
# Builds the contracts in the pinned Foundry image, as the calling user, with forge's
# home on .cache/foundry. Commands that deploy or verify read contracts/out, so they
# build first: what they deploy or compare against is what the repository holds now.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
: "${FOUNDRY_IMAGE:?FOUNDRY_IMAGE is not set; run through make}"
mkdir -p "$root/.cache/foundry"
out="$(docker run --rm -u "$(id -u):$(id -g)" -e HOME=/cache -v "$root/.cache/foundry:/cache" \
  -v "$root/contracts:/work" -w /work --entrypoint forge "$FOUNDRY_IMAGE" build 2>&1)" \
  || { printf '%s\n' "$out" | tail -30; echo "build: FAIL"; exit 1; }
[ -f "$root/contracts/out/Vault.sol/Vault.json" ] || { echo "build: FAIL: no artifact for Vault"; exit 1; }
echo "build: ok"
