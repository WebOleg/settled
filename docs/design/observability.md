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

Grafana 13.3.2, pinned by digest, at `http://127.0.0.1:3000`. The dashboard
[settled: chain](../../infra/observability/grafana/dashboards/chain.json) opens as the
home page.

| Panel | What it shows |
|---|---|
| L2 heads | Unsafe: built by the sequencer. Safe: derived from batches on the L1. Finalized: its batch is in a finalized L1 block. One line per node and level. |
| Blocks behind the unsafe head | How far safe and finalized trail unsafe, per node. Safe trails by about one batch (30 s, 15 blocks); finalized by L1 finality (about 10 minutes, 300 blocks). |
| Verifier behind the sequencer | Unsafe head of the sequencer minus the verifier's, in blocks. 0 to 1 is normal; a growing value means the verifier stopped following. |
| L2 block rate | New unsafe blocks per minute on the sequencer. The block time is 2 s, so 30. |
| Safe lag | Unsafe minus safe on the verifier, in seconds of L2 time (2 s per block). |
| Op-node peers | Peer-to-peer connections of each op-node. Each should have the other one. |
| L1 heads | geth's head, safe and finalized blocks. |
| Validator duties | Blocks and attestations signed by the 64 validators, per minute. |
| Blocks waiting to be batched | L2 blocks the batcher holds, by stage. A queue that only grows means batches are not leaving. |
| Since the last confirmed batch | Seconds since the batcher's last L1 transaction was confirmed. A channel closes every 30 s. |
| Batch transactions and blob fee | Confirmed batcher transactions per minute, and the L1 blob base fee they paid. |
| Since the last confirmed proposal | Seconds since the proposer's last L1 transaction was confirmed. A game is made when the finalized L2 head moves, about every 6 minutes here. |
| Age of the proposed state | Seconds between now and the L2 time of the last proposed super root. |
| Operator balances on the L1 | ETH left to pay for batches and proposals. Topped up by make l2-fund. |
| Gas used per L2 block | Gas used by the head block of each op-reth. |
| Problems | Things that should stay at zero: op-node derivation errors and op-reth reorgs over the last 15 minutes. |

| Choice | Reason |
|---|---|
| Dashboards and the data source as files, edits in the UI refused | What is shown is what is reviewed in the repository; a panel changed by hand would be lost and could not be reviewed. |
| Anonymous visitors are viewers | Anyone on this machine can open the dashboard without a password; nobody can change it without one. Grafana listens on 127.0.0.1 only. |
| Admin password created once by `obs-init.sh`, read by Grafana from a file | It is not in `compose.yaml`, an env file or `docker inspect`. The file is 644 because Grafana reads it as its own user inside the container; its folder is 700, so nobody else on this machine can reach it. `make obs-password` copies it to the clipboard without printing it. |
| Update checks, usage reporting, the news feed and Gravatar off | A local tool should not call out. |

`make obs-check` shows that all nine targets are up and each of the seven jobs reports
series; that Grafana is healthy and the admin password works; that an anonymous visitor
reads every panel and cannot write; and that every query of every panel returns data
from Prometheus. An empty panel fails the check, so a renamed metric is noticed at once.
