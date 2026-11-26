#!/usr/bin/env bash
# Shows that Prometheus scrapes every client of the devnet: each of the nine targets is
# up, and each job reports series. Then Grafana: it is healthy, the admin password from
# obs-init.sh works, an anonymous visitor can read the dashboard but not change it, and
# every query of every panel returns data. Waits up to 60 s for the first scrape.
set -euo pipefail

PROM="http://127.0.0.1:9090"
GRAFANA="http://127.0.0.1:3000"
EXPECTED_TARGETS=9
root="$(git rev-parse --show-toplevel)"
DASHBOARD="$root/infra/observability/grafana/dashboards/chain.json"
PASSWORD_FILE="$root/.devnet/observability/grafana-admin.txt"

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

# Grafana.
waited=0
until curl -sf -m 3 "$GRAFANA/api/health" >/dev/null 2>&1; do
  [ "$waited" -lt 60 ] || fail "Grafana does not answer on $GRAFANA after 60 s"
  sleep 3; waited=$((waited + 3))
done
echo "grafana          healthy, version $(curl -sf -m 3 "$GRAFANA/api/health" | jq -r '.version')"

# The password goes to curl on stdin, never on its command line.
admin_status="$(printf 'user = "admin:%s"\n' "$(tr -d '[:space:]' < "$PASSWORD_FILE")" \
  | curl -s -m 5 -K - -o /dev/null -w '%{http_code}' "$GRAFANA/api/user")"
[ "$admin_status" = "200" ] || fail "the admin password from obs-init.sh is refused (HTTP $admin_status)"
echo "admin            the password from .devnet/observability works"

uid="$(jq -r '.uid' "$DASHBOARD")"
dash="$(curl -sf -m 5 "$GRAFANA/api/dashboards/uid/$uid")" || fail "anonymous read of dashboard $uid refused"
panels="$(printf '%s' "$dash" | jq '[.dashboard.panels[] | select(.type != "row")] | length')"
want="$(jq '[.panels[] | select(.type != "row")] | length' "$DASHBOARD")"
[ "$panels" -eq "$want" ] || fail "Grafana shows $panels panels, the file has $want"
write_status="$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"dashboard":{"title":"anonymous write"},"overwrite":false}' "$GRAFANA/api/dashboards/db")"
case "$write_status" in 401|403) ;; *) fail "an anonymous visitor could write a dashboard (HTTP $write_status)" ;; esac
echo "viewer           anonymous visitors read the $panels panels and cannot write (HTTP $write_status)"

# Every query of every panel must return data now; an empty panel is a broken panel.
empty=0
while IFS=$'\t' read -r title expr; do
  n="$(query "$expr" | jq '.data.result | length')" || n=0
  if [ "$n" -eq 0 ]; then
    echo "empty            $title: $expr"
    empty=$((empty + 1))
  fi
done < <(jq -r '.panels[] | select(.type != "row") | .title as $t | .targets[] | [$t, .expr] | @tsv' "$DASHBOARD")
queries="$(jq '[.panels[] | select(.type != "row") | .targets[]] | length' "$DASHBOARD")"
[ "$empty" -eq 0 ] || fail "$empty of $queries panel queries return no data"
echo "panels           all $queries queries of the $panels panels return data"

echo "obs-check: ok"
