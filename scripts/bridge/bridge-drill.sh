#!/usr/bin/env bash
# Proves that the bridge's alerts reach someone, not only that the rules load. Stops the
# ledger's PostgreSQL, so every step of the deposit indexer fails while the indexer
# itself keeps running and reporting; waits for DepositIndexerStale to fire and reach
# Alertmanager. Then starts PostgreSQL again and waits for the alert to clear. A stopped
# indexer would take its metrics with it, which TargetDown covers; a database that is
# down is the failure only this alert sees. PostgreSQL is started again on any exit. No
# deposit is lost: the indexer goes on from the last block it stored.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

PROM="http://127.0.0.1:9090"
ALERTMANAGER="http://127.0.0.1:9093"
BRIDGE="docker compose -f infra/bridge/compose.yaml"
ALERT="DepositIndexerStale"
LIMIT=300

fail() { echo "bridge-drill: FAIL: $*"; exit 1; }
in_prometheus() {
  curl -sf -m 5 "$PROM/api/v1/alerts" | jq --arg a "$1" --arg s "$2" \
    '[.data.alerts[] | select(.labels.alertname == $a and .state == $s)] | length'
}
in_alertmanager() {
  curl -sf -m 5 "$ALERTMANAGER/api/v2/alerts" | jq --arg a "$1" '[.[] | select(.labels.alertname == $a)] | length'
}
age() {
  curl -sf -m 5 --data-urlencode 'query=time() - max(deposit_indexer_last_step_timestamp_seconds{chain="l2"})' \
    "$PROM/api/v1/query" | jq -r '.data.result[0].value[1] // "?" | if . == "?" then . else tonumber | floor | tostring end'
}

echo "== Before =="
for s in postgres deposit-indexer; do
  [ "$($BRIDGE ps --format '{{.State}}' "$s")" = "running" ] || fail "$s is not running"
done
firing="$(curl -sf -m 5 "$PROM/api/v1/alerts" | jq -r '[.data.alerts[] | select(.state == "firing") | .labels.alertname] | unique | join(" ")')" \
  || fail "Prometheus does not answer on $PROM"
[ -z "$firing" ] || fail "alerts already firing: $firing"
curl -sf -m 5 "$ALERTMANAGER/-/ready" >/dev/null || fail "Alertmanager does not answer on $ALERTMANAGER"
echo "bridge           no alert firing; last step $(age) s ago"

restart() { $BRIDGE start postgres >/dev/null 2>&1 || true; }
trap restart EXIT

echo
echo "== PostgreSQL stopped =="
$BRIDGE stop postgres >/dev/null 2>&1 || fail "could not stop postgres"
start="$(date +%s)"
echo "stopped          postgres at $(date '+%H:%M:%S'); the alert fires after 60 s without a step plus 1 min held"
until [ "$(in_prometheus "$ALERT" firing)" -ge 1 ]; do
  t=$(($(date +%s) - start))
  [ "$t" -lt "$LIMIT" ] || fail "$ALERT did not fire in $LIMIT s"
  sleep 10
done
echo "fired            $ALERT after $(($(date +%s) - start)) s; last step $(age) s ago"
t0="$(date +%s)"
until [ "$(in_alertmanager "$ALERT")" -ge 1 ]; do
  [ $(($(date +%s) - t0)) -lt 60 ] || fail "$ALERT fires in Prometheus but did not reach Alertmanager in 60 s"
  sleep 5
done
echo "alertmanager     received $ALERT after $(($(date +%s) - t0)) s more"

echo
echo "== PostgreSQL started =="
$BRIDGE start postgres >/dev/null 2>&1 || fail "could not start postgres"
trap - EXIT
start="$(date +%s)"
echo "started          postgres at $(date '+%H:%M:%S')"
until [ "$(in_prometheus "$ALERT" firing)" -eq 0 ] && [ "$(in_prometheus "$ALERT" pending)" -eq 0 ] && [ "$(in_alertmanager "$ALERT")" -eq 0 ]; do
  t=$(($(date +%s) - start))
  [ "$t" -lt "$LIMIT" ] || fail "$ALERT did not clear in $LIMIT s"
  sleep 5
done
echo "cleared          $ALERT after $(($(date +%s) - start)) s; last step $(age) s ago"
echo "bridge-drill: ok"
