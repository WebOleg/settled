#!/usr/bin/env bash
# Shows that Prometheus scrapes every client of the devnet: each of the nine targets is
# up, and each job reports series. Then Grafana: it is healthy, the admin password from
# obs-init.sh works, an anonymous visitor can read the dashboard but not change it, and
# every query of every panel returns data and shows only live series. Waits up to 60 s for
# the first scrape.
set -euo pipefail

PROM="http://127.0.0.1:9090"
GRAFANA="http://127.0.0.1:3000"
EXPECTED_TARGETS=9
root="$(git rev-parse --show-toplevel)"
DASHBOARD="$root/infra/observability/grafana/dashboards/chain.json"
PASSWORD_FILE="$root/.devnet/observability/grafana-admin.txt"

fail() { echo "obs-check: FAIL: $*"; exit 1; }
query() { curl -sf -m 5 --data-urlencode "query=$1" "$PROM/api/v1/query"; }
# The same query over the 30 minutes the dashboard shows by default.
query_range() {
  local now; now="$(date +%s)"
  curl -sf -m 10 --data-urlencode "query=$1" --data-urlencode "start=$((now - 1800))" \
    --data-urlencode "end=$now" --data-urlencode "step=30" "$PROM/api/v1/query_range"
}

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
# Panels inside collapsed rows are kept in the row; count them too.
panels="$(printf '%s' "$dash" | jq '[.dashboard.panels[] | if .type == "row" then (.panels // [])[] else . end] | length')"
want="$(jq '[.panels[] | if .type == "row" then (.panels // [])[] else . end] | length' "$DASHBOARD")"
[ "$panels" -eq "$want" ] || fail "Grafana shows $panels panels, the file has $want"
write_status="$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"dashboard":{"title":"anonymous write"},"overwrite":false}' "$GRAFANA/api/dashboards/db")"
case "$write_status" in 401|403) ;; *) fail "an anonymous visitor could write a dashboard (HTTP $write_status)" ;; esac
echo "viewer           anonymous visitors read the $panels panels and cannot write (HTTP $write_status)"

# Every query of every panel must return data now; an empty panel is a broken panel.
# Over the 30 minutes on screen it must show the same series as now: a series that has
# stopped (left behind when a target label is renamed, say) would draw a second line and
# a second number in a tile. Each selector names its chain, which keeps such series out.
empty=0
stale=0
unpinned=0
while IFS=$'\t' read -r title expr; do
  n="$(query "$expr" | jq '.data.result | length')" || n=0
  if [ "$n" -eq 0 ]; then
    echo "empty            $title: $expr"
    empty=$((empty + 1))
  fi
  r="$(query_range "$expr" | jq '.data.result | length')" || r=0
  if [ "$r" -ne "$n" ]; then
    echo "stale            $title: $r series over 30 min, $n now: $expr"
    stale=$((stale + 1))
  fi
  selectors="$(printf '%s' "$expr" | grep -o '{' | wc -l | tr -d ' ')"
  pinned="$(printf '%s' "$expr" | grep -o 'chain="l[12]"' | wc -l | tr -d ' ')"
  if [ "$selectors" -ne "$pinned" ]; then
    echo "unpinned         $title: $expr"
    unpinned=$((unpinned + 1))
  fi
done < <(jq -r '[.panels[] | if .type == "row" then (.panels // [])[] else . end][] | .title as $t | .targets[] | [$t, .expr] | @tsv' "$DASHBOARD")
queries="$(jq '[[.panels[] | if .type == "row" then (.panels // [])[] else . end][] | .targets[]] | length' "$DASHBOARD")"
[ "$empty" -eq 0 ] || fail "$empty of $queries panel queries return no data"
[ "$unpinned" -eq 0 ] || fail "$unpinned of $queries panel queries have a selector without chain=\"l1\" or chain=\"l2\""
[ "$stale" -eq 0 ] || fail "$stale of $queries panel queries show series that have stopped"
echo "panels           all $queries queries of the $panels panels return data, name their chain and show only live series"

# The home page is this dashboard, so whoever opens Grafana sees the six questions first.
# A home dashboard read from a file is served as a copy under Grafana's own uid
# (default-home-dashboard), so the page is matched by uid, by title, or by the slug
# Grafana makes from the title in its redirect ("settled: chain" -> "settled3a-chain").
title="$(jq -r '.title' "$DASHBOARD")"
home="$(curl -sf -m 5 "$GRAFANA/api/dashboards/home")" || fail "Grafana does not return its home page"
printf '%s' "$home" | jq -e --arg uid "$uid" --arg title "$title" '
  ($title | ascii_downcase | gsub(":"; "3a") | gsub("[^a-z0-9]+"; "-") | gsub("^-|-$"; "")) as $slug
  | ((.redirectUri // "") | sub("[?].*$"; "") | split("/")) as $path
  | ($path[1] == "d" and ($path[2] == $uid or $path[3] == $slug))
    or (.dashboard.uid == $uid) or (.dashboard.title == $title)' >/dev/null \
  || fail "the Grafana home page is not the $uid dashboard: $(printf '%s' "$home" | jq -c '{redirectUri, uid: .dashboard.uid, title: .dashboard.title}')"
echo "home             $GRAFANA opens the $uid dashboard; direct link $GRAFANA/d/$uid"

# Overview tiles: each must show exactly one number now. Its colour is worked out from the
# tile's own thresholds in the dashboard file, the same way Grafana colours it.
red=0
tiles=0
while IFS=$'\t' read -r title expr steps unit decimals; do
  tiles=$((tiles + 1))
  result="$(query "$expr")" || fail "overview tile '$title': query failed"
  n="$(printf '%s' "$result" | jq '.data.result | length')"
  [ "$n" -eq 1 ] || fail "overview tile '$title' shows $n numbers, want 1"
  v="$(printf '%s' "$result" | jq -r '.data.result[0].value[1]')"
  colour="$(jq -rn --argjson v "$v" --argjson s "$steps" '[$s[] | select(.value == null or .value <= $v)] | last | .color')"
  shown="$(jq -rn --argjson v "$v" --argjson d "$decimals" --arg u "$unit" \
    '(($v * pow(10; $d) | round) / pow(10; $d)) as $r
     | (if $d > 0 then ($r | tostring | if test("[.]") then . else . + "." end) as $t
          | ($t | split(".")) as $p | $p[0] + "." + ($p[1] + "00")[0:$d]
        else ($r | floor | tostring) end)
       + (if $u == "s" then " s" else ($u | sub("^suffix:"; "")) end)')"
  printf 'tile             %-22s %-6s %s\n' "$title" "$colour" "$shown"
  [ "$colour" = "green" ] || red=$((red + 1))
done < <(jq -r '.panels[] | select(.type == "stat") | [.title, .targets[0].expr, (.fieldConfig.defaults.thresholds.steps | tojson), .fieldConfig.defaults.unit, (.fieldConfig.defaults.decimals // 0)] | @tsv' "$DASHBOARD")
echo "overview         $((tiles - red)) of $tiles tiles green"

echo "obs-check: ok"
