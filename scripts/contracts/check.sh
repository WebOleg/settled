#!/usr/bin/env bash
# Checks the contracts in the pinned Foundry image: formatting, a build without compiler
# warnings, an artifact for every source file (forge in a broken image once exited 0 and
# built nothing), and the tests when there are any. Run as the calling user, with HOME on
# .cache/foundry, so files in the repository stay owned by that user and solc is
# downloaded once.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
: "${FOUNDRY_IMAGE:?FOUNDRY_IMAGE is not set; run through make}"
mkdir -p .cache/foundry

forge() {
  docker run --rm -u "$(id -u):$(id -g)" -e HOME=/cache -v "$root/.cache/foundry:/cache" \
    -v "$root/contracts:/work" -w /work --entrypoint forge "$FOUNDRY_IMAGE" "$@"
}
fail() { echo "check-contracts: FAIL: $*"; exit 1; }

[ -f contracts/lib/forge-std/src/Test.sol ] || fail "contracts/lib is empty; run git submodule update --init"

forge fmt --check >/dev/null 2>&1 || { forge fmt --check 2>&1 | head -40; fail "not formatted; forge fmt fixes it"; }

out="$(forge build --force 2>&1)" || { printf '%s\n' "$out" | tail -30; fail "build failed"; }
case "$out" in
  *"with warnings"*) printf '%s\n' "$out" | tail -30; fail "the compiler warns" ;;
esac

missing=0
while IFS= read -r src; do
  name="$(basename "$src")"
  [ -n "$(find "contracts/out/$name" -name '*.json' 2>/dev/null | head -1)" ] || { echo "no artifact for $src"; missing=$((missing + 1)); }
done < <(cd contracts && find src -name '*.sol' | sort)
[ "$missing" -eq 0 ] || fail "$missing source files built no artifact"
sources="$(cd contracts && find src -name '*.sol' | wc -l | tr -d ' ')"

if [ -n "$(cd contracts && find test -name '*.t.sol' 2>/dev/null | head -1)" ]; then
  forge test || fail "tests failed"
fi
echo "check-contracts: ok ($sources source files built)"
