#!/usr/bin/env bash
# Checks the contracts in the pinned Foundry image: formatting, a build without compiler
# warnings, an artifact for every source file (forge in a broken image once exited 0 and
# built nothing), forge lint on src/ with no finding, the tests, the gas of the unit
# tests against .gas-snapshot, and the
# symbolic tests with Halmos, in its own image (infra/images/halmos), built here when the
# image for the current Dockerfile and requirements is missing. Run as the calling user, with HOME on
# .cache/foundry, so files in the repository stay owned by that user and solc is
# downloaded once.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
: "${FOUNDRY_IMAGE:?FOUNDRY_IMAGE is not set; run through make}"
mkdir -p .cache/foundry

forge() {
  docker run --rm -u "$(id -u):$(id -g)" -e HOME=/cache -e FOUNDRY_PROFILE="${FOUNDRY_PROFILE:-default}" \
    -v "$root/.cache/foundry:/cache" \
    -v "$root/contracts:/work" -w /work --entrypoint forge "$FOUNDRY_IMAGE" "$@"
}
fail() { echo "check-contracts: FAIL: $*"; exit 1; }

[ -f contracts/lib/forge-std/src/Test.sol ] || fail "contracts/lib is empty; run git submodule update --init"

forge fmt --check >/dev/null 2>&1 || { forge fmt --check 2>&1 | head -40; fail "not formatted; forge fmt fixes it"; }

out="$(forge build --force 2>&1)" || { printf '%s\n' "$out" | tail -30; fail "build failed"; }
case "$out" in
  *"with warnings"*) printf '%s\n' "$out" | tail -30; fail "the compiler warns" ;;
esac

# Lint findings are fixed, or suppressed in the source on the line they concern with
# the reason next to it; none may be left standing.
lint="$(forge lint src 2>&1 || true)"
case "$lint" in
  *"warning["* | *"error["*) printf '%s\n' "$lint" | tail -40; fail "forge lint reports findings in src/" ;;
esac

missing=0
while IFS= read -r src; do
  name="$(basename "$src")"
  [ -n "$(find "contracts/out/$name" -name '*.json' 2>/dev/null | head -1)" ] || { echo "no artifact for $src"; missing=$((missing + 1)); }
done < <(cd contracts && find src -name '*.sol' | sort)
[ "$missing" -eq 0 ] || fail "$missing source files built no artifact"
sources="$(cd contracts && find src -name '*.sol' | wc -l | tr -d ' ')"

if [ -n "$(cd contracts && find test -name '*.t.sol' 2>/dev/null | head -1)" ]; then
  out="$(forge test 2>&1)" || { printf '%s\n' "$out" | tail -60; fail "tests failed"; }
  printf '%s\n' "$out" | grep -E '^Ran [0-9]+ test suite' | tail -1
  echo "profile          ${FOUNDRY_PROFILE:-default}"
fi
if [ -f contracts/.gas-snapshot ]; then
  out="$(forge snapshot --check --match-path 'test/unit/*' 2>&1)" || { printf '%s\n' "$out" | grep -iE 'diff|changed|mismatch|fail' | head -20; fail "gas differs from .gas-snapshot; run make contracts-snapshot and review the change"; }
fi
# Halmos runs on linux/amd64 (no arm64 wheel of the z3 it pins) and with its own home:
# forge inside it downloads an amd64 solc, which must not land in the native cache.
# Built here from pinned bases; the tag is the hash of what it is built from.
halmos_tag="settled-halmos:$(cat infra/images/halmos/Dockerfile infra/images/halmos/requirements.txt | shasum -a 256 | cut -c1-12)"
if ! docker image inspect "$halmos_tag" >/dev/null 2>&1; then
  echo "halmos image     building $halmos_tag"
  docker build -q --platform linux/amd64 -t "$halmos_tag" infra/images/halmos >/dev/null || fail "could not build the Halmos image"
fi
mkdir -p .cache/halmos
out="$(docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -e HOME=/cache -v "$root/.cache/halmos:/cache" \
  -v "$root/contracts:/work" -w /work "$halmos_tag" \
  --solver z3 --match-contract VaultSymbolicTest --solver-timeout-assertion 60000 2>&1)" \
  || { printf '%s\n' "$out" | grep -vE '^\s*$' | tail -30; fail "symbolic tests failed"; }
printf '%s\n' "$out" | grep -E '^Symbolic test result' | tail -1
echo "check-contracts: ok, $sources source files built, tests passed"
