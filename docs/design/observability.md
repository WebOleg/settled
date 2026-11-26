# Design: observability

How the devnet is watched and why. This file covers what exists in the repository:
metrics from every client, collected by Prometheus, and a dashboard of the chain in
Grafana. Alerts with runbooks and load tests are added in the next steps of stage 3 and
described here as they land.

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
| Labels `chain`, `role`, `node` on every target | Dashboards and alerts group by them: the same metric from the sequencer and the verifier is compared by `node`. The first version called the first one `layer`, which collided with op-node's own `layer` label (`l1`, `l2`, `l1_origin`): Prometheus renamed op-node's to `exported_layer`. Target labels must not reuse a name the client already uses. |
| Prometheus v3.16.0, pinned by digest | The current stable release. Its config is checked with the `promtool` of the same image before every start. |
| Seven days of retention in a Docker volume | Enough for a week of devnet history; `make reset` deletes it with the chain it describes. |
| Started by `make up`, stopped first by every `down` | It joins the networks of both layers, which cannot be removed while it is attached. |

## Dashboard

Grafana 13.3.2, pinned by digest, at `http://127.0.0.1:3000`; the dashboard
[settled: chain](../../infra/observability/grafana/dashboards/chain.json) is the home
page (`http://127.0.0.1:3000/d/settled-chain`).

### How it is laid out

A person on duty takes in a handful of things at a glance, not twenty graphs. The first
version put sixteen panels on one screen, and it was hard to see where to look. So the
dashboard follows three rules:

1. One screen answers "is the chain healthy?" with six tiles. Each tile asks one
   question in plain words, shows one number, and is green or red. There is no third
   colour to interpret.
2. Everything else sits below in rows that stay closed until opened. Each tile says
   which row to open when it is red.
3. The red line of a tile is the threshold of the alert for the same question (next
   step of stage 3), so a red tile, an alert and its runbook are one thing.

| Tile | Green when | Red means |
|---|---|---|
| L2 makes blocks | The sequencer makes about 30 blocks a minute (one every 2 s). | Fewer than 25. |
| Batches reach the L1 | The last batch was confirmed on the L1 less than 2 minutes ago; a batch goes out every 30 s. | Older. |
| Verifier keeps up | The verifier is fewer than 5 blocks behind the sequencer. | It has fallen behind. |
| Proposals are made | The proposer posted a state to the L1 less than 15 minutes ago; it does about every 6. | Older. |
| L1 finalizes | The L1 finalized block is fewer than 160 blocks (5 epochs) behind its head; normally about 64 to 96. | Finality has stalled. |
| Operators have funds | The batcher and the proposer each hold at least 1 ETH on the L1. | One of them is running out. |

| Closed row | Panels |
|---|---|
| Details: L2 | L2 heads, Blocks behind the unsafe head, Op-node peers |
| Details: L1 | L1 heads, Validator duties |
| Details: batcher | Blocks waiting to be batched, Batch transactions and blob fee |
| Details: proposer and funds | Age of the proposed state, Operator balances on the L1 |
| Details: execution | Gas used per L2 block, Problems |

| Choice | Reason |
|---|---|
| Dashboards and the data source as files, edits in the UI refused | What is shown is what is reviewed in the repository; a panel changed by hand would be lost and could not be reviewed. |
| Anonymous visitors are viewers | Anyone on this machine can open the dashboard without a password; nobody can change it without one. Grafana listens on 127.0.0.1 only. |
| Admin password created once by `obs-init.sh`, read by Grafana from a file | It is not in `compose.yaml`, an env file or `docker inspect`. The file is 644 because Grafana reads it as its own user inside the container; its folder is 700, so nobody else on this machine can reach it. `make obs-password` copies it to the clipboard without printing it. |
| Update checks, usage reporting, the news feed and Gravatar off | A local tool should not call out. |

`make obs-check` shows that all nine targets are up and each of the seven jobs reports
series; that Grafana is healthy, the admin password works and the home page is this
dashboard; that an anonymous visitor reads every panel and cannot write; that every query
of every panel, closed rows included, returns data from Prometheus; and it prints each
tile with its colour, worked out from the tile's thresholds in the file. An empty panel
fails the check, so a renamed metric is noticed at once.
