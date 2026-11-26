# Design: observability

How the devnet is watched and why. This file covers what exists in the repository:
metrics from every client, collected by Prometheus. Dashboards, alerts with runbooks
and load tests are added in the steps of stage 3 and described here as they land.

## Goal

An operator should see, without reading logs, whether the chain is healthy: blocks are
built, batches reach the L1, the verifier agrees, proposals are made and the operator
accounts can pay for all of it. Each later alert reads a metric collected here.

## Metrics

Every client exposes Prometheus metrics inside its Docker network. Prometheus joins the
L1 and L2 networks and scrapes all nine every 5 seconds.

| Client | Flags | Endpoint inside Docker |
|---|---|---|
| geth | `--metrics --metrics.addr=0.0.0.0 --metrics.port=6060` | `geth:6060/debug/metrics/prometheus` |
| Lighthouse beacon node | `--metrics --metrics-address=0.0.0.0 --metrics-port=5054` | `beacon:5054/metrics` |
| Lighthouse validator client | `--metrics --metrics-address=0.0.0.0 --metrics-port=5064` | `validator:5064/metrics` |
| op-reth, sequencer and verifier | `--metrics=0.0.0.0:9001` | `op-reth:9001`, `verifier-op-reth:9001` |
| op-node, sequencer and verifier | `--metrics.enabled --metrics.port=7300` | `op-node:7300`, `verifier-op-node:7300` |
| op-batcher | `--metrics.enabled --metrics.port=7300` | `op-batcher:7300` |
| op-proposer | `--metrics.enabled --metrics.port=7300` | `op-proposer:7300` |

| Choice | Reason |
|---|---|
| Metrics ports not published | Metrics reveal balances, peers and internals. Only Prometheus reads them, from inside the Docker networks; only Prometheus itself is published, on 127.0.0.1:9090. |
| Scrape every 5 s | The L2 makes a block every 2 s and the L1 a slot every 6 s. The usual 15 s would hide most of what happens between two scrapes. |
| Labels `layer`, `role`, `node` on every target | Dashboards and alerts group by them: the same metric from the sequencer and the verifier is compared by `node`. |
| Prometheus v3.16.0, pinned by digest | The current stable release. Its config is checked with the `promtool` of the same image before every start. |
| Seven days of retention in a Docker volume | Enough for a week of devnet history; `make reset` deletes it with the chain it describes. |
| Started by `make up`, stopped first by every `down` | It joins the networks of both layers, which cannot be removed while it is attached. |

`make obs-check` shows that all nine targets are up and that each of the seven jobs
reports series.
