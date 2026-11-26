# Observability

Prometheus scraping every client of the devnet, and Grafana with a dashboard of the
chain. Why it is built this way is in
[docs/design/observability.md](../../docs/design/observability.md).

It needs the running L1 and L2 (`make up` starts everything, this included).

## Commands

| Command | What it does |
|---|---|
| `make obs-up` | Creates the Grafana admin password on first run, checks the Prometheus config with `promtool`, then starts Prometheus and Grafana |
| `make obs-check` | Shows that all nine targets are up, Grafana works, anonymous visitors can read but not write, and every panel query returns data |
| `make obs-password` | Copies the Grafana admin password to the clipboard without printing it |
| `make obs-down` | Stops Prometheus and Grafana and keeps their data |

## Endpoints

| Endpoint | Address |
|---|---|
| Prometheus | `http://127.0.0.1:9090` |
| Grafana | `http://127.0.0.1:3000`, the chain dashboard as home page |

## Files

| Path | Content |
|---|---|
| [prometheus.yml](prometheus.yml) | Scrape targets with their `chain`, `role` and `node` labels |
| [grafana/dashboards/chain.json](grafana/dashboards/chain.json) | The chain dashboard |
| [grafana/provisioning](grafana/provisioning) | The data source and the dashboard folder Grafana loads at start |
| `.devnet/observability/grafana-admin.txt` | Grafana admin password, created once; folder mode 700 |
| Docker volumes `settled-obs_prometheus`, `settled-obs_grafana` | Seven days of metrics, Grafana's own state; deleted by `make reset` |
