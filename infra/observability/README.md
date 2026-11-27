# Observability

Prometheus scraping every client of the devnet and evaluating the alert rules,
Alertmanager receiving the alerts, and Grafana with a dashboard of the chain. Why it is built this way is in
[docs/design/observability.md](../../docs/design/observability.md).

It needs the running L1, L2 and bridge (`make up` starts everything, this included).

## Commands

| Command | What it does |
|---|---|
| `make obs-up` | Creates the Grafana admin password on first run, checks the Prometheus config and rules with `promtool` and the Alertmanager config with `amtool`, then starts all three |
| `make obs-check` | Shows that all fourteen targets are up, Grafana works, anonymous visitors can read but not write, every panel query returns data, every alert rule matches its tile and has a runbook, and no alert fires |
| `make alert-drill` | Stops op-batcher, shows its alerts fire and reach Alertmanager, starts it, shows them clear |
| `make obs-password` | Copies the Grafana admin password to the clipboard without printing it |
| `make obs-down` | Stops Prometheus, Alertmanager and Grafana and keeps their data |

## Endpoints

| Endpoint | Address |
|---|---|
| Prometheus | `http://127.0.0.1:9090` |
| Alertmanager | `http://127.0.0.1:9093` |
| Grafana | `http://127.0.0.1:3000`, the chain dashboard as home page |

## Files

| Path | Content |
|---|---|
| [prometheus.yml](prometheus.yml) | Scrape targets with their `chain`, `role` and `node` labels |
| [alerts.yml](alerts.yml) | Alert rules, one per dashboard tile, and TargetDown; runbooks in [docs/runbooks.md](../../docs/runbooks.md) |
| [alertmanager.yml](alertmanager.yml) | Where alerts go: one receiver that keeps them |
| [grafana/dashboards/chain.json](grafana/dashboards/chain.json) | The chain dashboard |
| [grafana/provisioning](grafana/provisioning) | The data source and the dashboard folder Grafana loads at start |
| `.devnet/observability/grafana-admin.txt` | Grafana admin password, created once; folder mode 700 |
| Docker volumes `settled-obs_prometheus`, `settled-obs_alertmanager`, `settled-obs_grafana` | Seven days of metrics, Alertmanager's state, Grafana's own state; deleted by `make reset` |
