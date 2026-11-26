# Observability

Prometheus scraping every client of the devnet. Why it is built this way is in
[docs/design/observability.md](../../docs/design/observability.md).

It needs the running L1 and L2 (`make up` starts everything, this included).

## Commands

| Command | What it does |
|---|---|
| `make obs-up` | Checks the Prometheus config with `promtool`, then starts Prometheus |
| `make obs-check` | Shows that all nine targets are up and each job reports series |
| `make obs-down` | Stops Prometheus and keeps its data |

## Endpoints

| Endpoint | Address |
|---|---|
| Prometheus | `http://127.0.0.1:9090` |

## Files

| Path | Content |
|---|---|
| [prometheus.yml](prometheus.yml) | Scrape targets with their `layer`, `role` and `node` labels |
| Docker volume `settled-obs_prometheus` | Seven days of metrics; deleted by `make reset` |
