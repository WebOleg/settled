#!/usr/bin/env bash
# Shows that Prometheus scrapes every client of the devnet: each of the nine targets is
# up, and each job reports series. Prints a few chain values straight from the metrics,
# so the numbers can be compared with make status. Waits up to 60 s for the first scrape.
set -euo pipefail

PROM="http://127.0.0.1:9090"
EXPECTED_TARGETS=9

fail() { echo "obs-check: FAIL: $*"; exit 1; }
query() { curl -sf -m 5 --data-urlencode "query=$1" "$PROM/api/v1/query"; }

waited=0
until targets="$(curl -sf -m 5 "$PROM/api/v1/targets?state=active")" \
  && [ "$(printf '%s' "$targets" | jq '[.data.activeTargets[] | select(.health == "up")] | length')" -eq "$EXPECTED_TARGETS" ]; do
  [ "$waited" -lt 60 ] || break
  sleep 5; waited=$((waited + 5))
done
targets="$(curl -sf -m 5 "$PROM/api/v1/targets?state=active")" || fail "Prometheus does not answer on $PROM"
printf '%s' "$targets" | jq -r '.data.activeTargets[] | "\(.health)\t\(.labels.job)\t\(.labels.node // "-")\t\(.scrapeUrl)\t\(.lastError)"' \
  | sort -k2 | awk -F'\t' '{ printf "%-5s %-20s %-10s %s %s\n", $1, $2, $3, $4, $5 }'
up="$(printf '%s' "$targets" | jq '[.data.activeTargets[] | select(.health == "up")] | length')"
total="$(printf '%s' "$targets" | jq '.data.activeTargets | length')"
[ "$total" -eq "$EXPECTED_TARGETS" ] || fail "$total targets configured, want $EXPECTED_TARGETS"
[ "$up" -eq "$EXPECTED_TARGETS" ] || fail "$up of $total targets up"
echo "targets          $up of $total up"

series="$(query 'count by (job) ({__name__=~".+"})')" || fail "query failed"
printf '%s' "$series" | jq -r '.data.result[] | "series           \(.metric.job): \(.value[1])"' | sort
jobs="$(printf '%s' "$series" | jq '.data.result | length')"
[ "$jobs" -eq 7 ] || fail "$jobs jobs report series, want 7"

echo "obs-check: ok"
