#!/usr/bin/env bash
# Runs Slither on contracts/ in its own image (infra/images/slither), built here for this
# machine's platform when the image for the current Dockerfile and requirements is
# missing, through scripts/contracts/slither_gate.py: without arguments it fails on any
# finding not reviewed in contracts/slither.db.json; with --record it rewrites that file
# from what Slither reports now, for after a review. Slither cleans and rebuilds;
# FOUNDRY_OUT and FOUNDRY_CACHE_PATH keep that inside the container, away from
# contracts/out.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
mkdir -p .cache/foundry
tag="settled-slither:$(cat infra/images/slither/Dockerfile infra/images/slither/requirements.txt | shasum -a 256 | cut -c1-12)"
if ! docker image inspect "$tag" >/dev/null 2>&1; then
  echo "slither image    building $tag"
  docker build -q -t "$tag" infra/images/slither >/dev/null
fi
exec docker run --rm -u "$(id -u):$(id -g)" -e HOME=/cache \
  -e FOUNDRY_OUT=/tmp/slither/out -e FOUNDRY_CACHE_PATH=/tmp/slither/cache \
  -v "$root/.cache/foundry:/cache" -v "$root/contracts:/work" \
  -v "$root/scripts/contracts/slither_gate.py:/gate.py:ro" -w /work \
  --entrypoint python "$tag" -I /gate.py "$@"
