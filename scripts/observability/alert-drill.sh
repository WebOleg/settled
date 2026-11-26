#!/usr/bin/env bash
# Proves that an alert reaches someone when the chain breaks, not only that the rules
# load. Stops op-batcher, so no batch reaches the L1 and the safe head stops; waits for
# BatchesNotReachingL1 to fire and reach Alertmanager, and for TargetDown on the batcher,
# whose own metrics are gone. Then starts op-batcher again and waits for both to clear.
# The batcher is started again on any exit. No data is lost: the sequencer keeps making
# blocks, and the batcher posts them all when it is back.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

PROM="http://127.0.0.1:9090"
ALERTMANAGER="http://127.0.0.1:9093"
L2="docker compose -f infra/devnet/l2/compose.yaml"
ALERT="BatchesNotReachingL1"
LIMIT=480

fail() { echo "alert-drill: FAIL: $*"; exit 1; }
# Number of alerts in Prometheus with this name, this state and, optionally, this job.
in_prometheus() {
  curl -sf -m 5 "$PROM/api/v1/alerts" | jq --arg a "$1" --arg s "$2" --arg j "${3:-}" \
    '[.data.alerts[] | select(.labels.alertname == $a and .state == $s and ($j == "" or .labels.job == $j))] | length'
}
in_alertmanager() {
  curl -sf -m 5 "$ALERTMANAGER/api/v2/alerts" | jq --arg a "$1" '[.[] | select(.labels.alertname == $a)] | length'
}
safe_lag() {
  curl -sf -m 5 --data-urlencode 'query=(max(op_node_default_refs_number{chain="l2",node="sequencer",layer="l2",type="l2_unsafe"}) - max(op_node_default_refs_number{chain="l2",node="sequencer",layer="l2",type="l2_safe"})) * 2' \
    "$PROM/api/v1/query" | jq -r '.data.result[0].value[1] // "?" | if . == "?" then . else tonumber | floor | tostring end'
}

echo "== Before =="
[ "$($L2 ps --format '{{.State}}' op-batcher)" = "running" ] || fail "op-batcher is not running"
firing="$(curl -sf -m 5 "$PROM/api/v1/alerts" | jq -r '[.data.alerts[] | select(.state == "firing") | .labels.alertname] | unique | join(" ")')" \
  || fail "Prometheus does not answer on $PROM"
[ -z "$firing" ] || fail "alerts already firing: $firing"
curl -sf -m 5 "$ALERTMANAGER/-/ready" >/dev/null || fail "Alertmanager does not answer on $ALERTMANAGER"
echo "devnet           no alert firing; L2 not yet on the L1: $(safe_lag) s"

restart() { $L2 start op-batcher >/dev/null 2>&1 || true; }
trap restart EXIT

echo
echo "== Batcher stopped =="
$L2 stop op-batcher >/dev/null 2>&1 || fail "could not stop op-batcher"
start="$(date +%s)"
echo "stopped          op-batcher at $(date '+%H:%M:%S'); the alert fires after 3 min of lag plus 1 min held"
last=""
next=0
until [ "$(in_prometheus "$ALERT" firing)" -ge 1 ]; do
  t=$(($(date +%s) - start))
  [ "$t" -lt "$LIMIT" ] || fail "$ALERT did not fire in $LIMIT s"
  state="pending $(in_prometheus "$ALERT" pending)"
  # One line a minute, or when the alert changes state.
  if [ "$state" != "$last" ] || [ "$t" -ge "$next" ]; then
    printf 'waiting %4d s    lag %s s, %s\n' "$t" "$(safe_lag)" "$state"
    last="$state"; next=$((t + 60))
  fi
  sleep 10
done
echo "fired            $ALERT after $(($(date +%s) - start)) s, L2 not yet on the L1: $(safe_lag) s"
t0="$(date +%s)"
until [ "$(in_alertmanager "$ALERT")" -ge 1 ]; do
  [ $(($(date +%s) - t0)) -lt 60 ] || fail "$ALERT fires in Prometheus but did not reach Alertmanager in 60 s"
  sleep 5
done
echo "alertmanager     received $ALERT after $(($(date +%s) - t0)) s more"
[ "$(in_prometheus TargetDown firing op-batcher)" -ge 1 ] || fail "TargetDown does not fire for the stopped op-batcher"
echo "fired            TargetDown for op-batcher (its own metrics are gone)"

echo
echo "== Batcher started =="
$L2 start op-batcher >/dev/null 2>&1 || fail "could not start op-batcher"
trap - EXIT
start="$(date +%s)"
echo "started          op-batcher at $(date '+%H:%M:%S')"
last=""
next=0
while :; do
  f="$(in_prometheus "$ALERT" firing)"; p="$(in_prometheus "$ALERT" pending)"
  d="$(in_prometheus TargetDown firing op-batcher)"; a="$(in_alertmanager "$ALERT")"
  [ "$f" -eq 0 ] && [ "$p" -eq 0 ] && [ "$d" -eq 0 ] && [ "$a" -eq 0 ] && break
  t=$(($(date +%s) - start))
  [ "$t" -lt "$LIMIT" ] || fail "alerts did not clear in $LIMIT s"
  state="$ALERT firing $f, in Alertmanager $a, TargetDown $d"
  if [ "$state" != "$last" ] || [ "$t" -ge "$next" ]; then
    printf 'waiting %4d s    lag %s s, %s\n' "$t" "$(safe_lag)" "$state"
    last="$state"; next=$((t + 60))
  fi
  sleep 10
done
echo "cleared          $ALERT and TargetDown after $(($(date +%s) - start)) s, L2 not yet on the L1: $(safe_lag) s"
echo "alert-drill: ok"
