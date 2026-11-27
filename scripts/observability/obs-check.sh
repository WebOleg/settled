#!/usr/bin/env bash
# Shows that Prometheus scrapes every client of the devnet, the vault exporter, the
# deposit indexer, and the withdrawal payer and signer: each of the thirteen targets is
# up, and each job reports series. Then
# Grafana: it is healthy, the admin password from obs-init.sh works, an anonymous
# visitor can read the dashboard but not change it, and every query of every panel
# returns data and shows only live series. Then the alerts: every rule is loaded,
# matches its tile and has a runbook, Alertmanager receives them, and none fires on a
# healthy devnet. Waits up to 60 s for the first scrape.
set -euo pipefail

PROM="http://127.0.0.1:9090"
GRAFANA="http://127.0.0.1:3000"
ALERTMANAGER="http://127.0.0.1:9093"
EXPECTED_TARGETS=13
root="$(git rev-parse --show-toplevel)"
DASHBOARD="$root/infra/observability/grafana/dashboards/chain.json"
RULES="$root/infra/observability/alerts.yml"
RUNBOOKS="$root/docs/runbooks.md"
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

# Series that Prometheus makes itself (ALERTS, ALERTS_FOR_STATE from the alert rules)
# have no job; only scraped series are counted here.
series="$(query 'count by (job) ({__name__=~".+", job!=""})')" || fail "query failed"
printf '%s' "$series" | jq -r '.data.result[] | "series           \(.metric.job): \(.value[1])"' | sort
jobs="$(printf '%s' "$series" | jq '.data.result | length')"
[ "$jobs" -eq 11 ] || fail "$jobs jobs report series, want 11"

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

# The home page is this dashboard, so whoever opens Grafana sees the overview first.
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

# Alerts. The rules file is read as written: one line per field, as alerts.yml keeps it.
rules="$(awk '
  function flush() { if (name != "") print name "\t" (tile == "" ? "-" : tile) "\t" expr "\t" runbook }
  /^ *- alert: / { flush(); name = $0; sub(/^ *- alert: /, "", name); tile = ""; expr = ""; runbook = "" }
  /^ *expr: / { expr = $0; sub(/^ *expr: /, "", expr) }
  /^ *tile: / { tile = $0; sub(/^ *tile: /, "", tile) }
  /^ *runbook: / { runbook = $0; sub(/^ *runbook: /, "", runbook) }
  END { flush() }' "$RULES")"
[ -n "$rules" ] || fail "no rules in $RULES"
count="$(printf '%s\n' "$rules" | wc -l | tr -d ' ')"

# Every tile has a rule with the tile's query and its red line, nothing else.
while IFS=$'\t' read -r title want; do
  got="$(printf '%s\n' "$rules" | awk -F'\t' -v t="$title" '$2 == t { print $3 }')"
  [ -n "$got" ] || fail "tile '$title' has no alert rule"
  [ "$got" = "$want" ] || fail "the rule of tile '$title' differs from the tile: rule '$got', tile '$want'"
done < <(jq -r '.panels[] | select(.type == "stat") | .fieldConfig.defaults.thresholds.steps as $s
  | [.title, (.targets[0].expr + (if $s[0].color == "red" then " < " else " >= " end) + ($s[1].value | tostring))] | @tsv' "$DASHBOARD")

# Every rule has a runbook section, headed by the alert's name.
while IFS=$'\t' read -r name _ _ runbook; do
  want="docs/runbooks.md#$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')"
  [ "$runbook" = "$want" ] || fail "rule $name links runbook '$runbook', want '$want'"
  grep -qx "## $name" "$RUNBOOKS" || fail "docs/runbooks.md has no section '## $name'"
done <<< "$rules"
echo "rules            $count rules in alerts.yml; each tile has the rule with its query and red line; each rule has a runbook"

loaded="$(curl -sf -m 5 "$PROM/api/v1/rules?type=alert")" || fail "Prometheus does not return its rules"
names_file="$(printf '%s\n' "$rules" | cut -f1 | sort | tr '\n' ' ')"
names_loaded="$(printf '%s' "$loaded" | jq -r '.data.groups[].rules[].name' | sort | tr '\n' ' ')"
[ "$names_file" = "$names_loaded" ] || fail "Prometheus loaded rules '$names_loaded', alerts.yml has '$names_file'"
bad="$(printf '%s' "$loaded" | jq -r '[.data.groups[].rules[] | select(.health != "ok") | .name] | join(" ")')"
[ -z "$bad" ] || fail "rules that do not evaluate: $bad"
echo "prometheus       evaluates all $count rules"

curl -sf -m 5 "$ALERTMANAGER/-/ready" >/dev/null || fail "Alertmanager does not answer on $ALERTMANAGER"
ams="$(curl -sf -m 5 "$PROM/api/v1/alertmanagers" | jq '.data.activeAlertmanagers | length')"
[ "$ams" -eq 1 ] || fail "Prometheus sends alerts to $ams Alertmanagers, want 1"
echo "alertmanager     ready at $ALERTMANAGER; Prometheus sends to it"

alerts="$(curl -sf -m 5 "$PROM/api/v1/alerts")" || fail "Prometheus does not return its alerts"
pending="$(printf '%s' "$alerts" | jq -r '[.data.alerts[] | select(.state == "pending") | .labels.alertname] | unique | join(" ")')"
firing="$(printf '%s' "$alerts" | jq -r '[.data.alerts[] | select(.state == "firing") | .labels.alertname] | unique | join(" ")')"
[ -z "$pending" ] || echo "pending          $pending"
[ -z "$firing" ] || fail "alerts firing on the devnet: $firing"
echo "alerts           none firing"

echo "obs-check: ok"
