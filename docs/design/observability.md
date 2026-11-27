# Design: observability

How the devnet is watched and why. This file covers what exists in the repository:
metrics from every client, collected by Prometheus, a dashboard of the chain in Grafana,
and alerts with runbooks, sent to Alertmanager. The load test, and what it showed about
the batcher, is in [load.md](load.md).

## Goal

An operator should see, without reading logs, whether the chain is healthy: blocks are
built, batches reach the L1, the verifier agrees, proposals are made and the operator
accounts can pay for all of it. Each later alert reads a metric collected here.

## Metrics

Every client exposes Prometheus metrics inside its Docker network. Prometheus joins the
L1 and L2 networks and scrapes all nine every 5 seconds, and the vault exporter and the
bridge's deposit indexer with them.

| Client | Flags | Endpoint inside Docker |
|---|---|---|
| geth | `--metrics --metrics.addr=0.0.0.0 --metrics.port=6060` | `geth:6060/debug/metrics/prometheus` |
| Lighthouse beacon node | `--metrics --metrics-address=0.0.0.0 --metrics-port=5054` | `beacon:5054/metrics` |
| Lighthouse validator client | `--metrics --metrics-address=0.0.0.0 --metrics-port=5064` | `validator:5064/metrics` |
| op-reth, sequencer and verifier | `--metrics=0.0.0.0:9001` | `op-reth:9001`, `verifier-op-reth:9001` |
| op-node, sequencer and verifier | `--metrics.enabled --metrics.port=7300` | `op-node:7300`, `verifier-op-node:7300` |
| op-batcher | `--metrics.enabled --metrics.port=7300` | `op-batcher:7300` |
| op-proposer | `--metrics.enabled --metrics.port=7300` | `op-proposer:7300` |
| vault-exporter ([cmd/vault-exporter](../../cmd/vault-exporter/main.go)) | `-listen=:9100` | `vault-exporter:9100/metrics` |
| deposit-indexer ([cmd/deposit-indexer](../../cmd/deposit-indexer/main.go)) | `-listen=:9110` | `deposit-indexer:9110/metrics`, on the bridge's network |

The vault exporter is built from this repository ([infra/images/vault-exporter](../../infra/images/vault-exporter/Dockerfile)).
It reads the vault from the verifier node, which derives the chain from the L1, every 2
seconds: whether it is paused, the ETH it holds and the ETH it accounts for (compared to
the wei, `vault_solvent`), and the withdrawals waiting in its queue, rebuilt from the
vault's events since its deployment block ([internal/vaultwatch](../../internal/vaultwatch/watch.go)).
If the block it last read leaves the chain, it reads the events again from the start. It
finds the vault in the deployment record, read at every poll from a folder mounted
read-only, which holds no key. With no vault deployed it reports nothing to watch, so no
alert fires.

| Choice | Reason |
|---|---|
| Metrics ports not published | Metrics reveal balances, peers and internals. Only Prometheus reads them, from inside the Docker networks; only Prometheus itself is published, on 127.0.0.1:9090. |
| Scrape every 5 s | The L2 makes a block every 2 s and the L1 a slot every 6 s. The usual 15 s would hide most of what happens between two scrapes. |
| Labels `chain`, `role`, `node` on every target | Dashboards and alerts group by them: the same metric from the sequencer and the verifier is compared by `node`. The first version called the first one `layer`, which collided with op-node's own `layer` label (`l1`, `l2`, `l1_origin`): Prometheus renamed op-node's to `exported_layer`. Target labels must not reuse a name the client already uses. Renaming the label also left the old series in the database until retention drops them, so every query names its chain (`chain="l2"`); a series without the label is never shown. |
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

1. One screen answers "is the chain healthy?" with six tiles, "is the vault safe?" with
   four more, "are deposits credited?" with four more, and "is suspense explained?"
   with one. Each tile asks one question in plain words, shows one number with its
   unit, and is green or red. There is no third colour to interpret. A tile reads the
   value now (an instant query) and aggregates it to one series, so it can never split
   into several numbers.
2. Everything else sits below in rows that stay closed until opened. Each tile says
   which row to open when it is red.
3. The red line of a tile is the threshold of the alert for the same question, so a red
   tile, an alert and its runbook are one thing (see Alerts).

| Tile | Green when | Red means |
|---|---|---|
| L2 makes blocks | The sequencer makes about 30 blocks a minute (one every 2 s). | Fewer than 25. |
| Batches reach the L1 | Less than 3 minutes of L2 blocks are not yet confirmed by the L1 (unsafe head minus safe head, 2 s a block); normally under one, as a batch goes out every 30 s. | Batches are not reaching the L1. |
| Verifier keeps up | The verifier is fewer than 5 blocks behind the sequencer. | It has fallen behind. |
| Proposals are made | The proposer posted a state to the L1 less than 15 minutes ago; it does about every 6. | Older. |
| L1 finalizes | The L1 finalized block is fewer than 160 blocks (5 epochs) behind its head; normally about 64 to 96. | Finality has stalled. |
| Operators have funds | The batcher and the proposer each hold at least 1 ETH on the L1. | One of them is running out. |
| Vault holds what it owes | The vault holds at least the ETH its deposits account for, to the wei. | It holds less. |
| Vault queue is empty | No large withdrawal waits in the queue. | One does: check it against the ledger before its release block. |
| Vault is open | Deposits and payments run. | The guardian has paused the vault. |
| Vault is watched | The vault exporter read the chain in the last minute. | It did not; the vault tiles show an old reading. |
| Deposits are indexed | The deposit indexer completed a step in the last minute. | It did not; no deposit is held, credited or voided. |
| Indexer keeps up | The indexer has read to within 30 blocks (a minute) of the node's head. | It is behind; new deposits are not yet held. |
| Finality holds | Every block the indexer holds final is still in the chain. | One is not; the indexer has stopped. |
| Ledger balances | Every balance equals the sum of its transfers, and debits equal credits. | They do not. |

| Closed row | Panels |
|---|---|
| Details: L2 | L2 heads, Blocks behind the unsafe head, Op-node peers |
| Details: L1 | L1 heads, Validator duties |
| Details: batcher | Blocks waiting to be batched, Batch transactions and blob fee |
| Details: proposer and funds | Age of the proposed state, Operator balances on the L1 |
| Details: execution | Gas used per L2 block, Problems |
| Details: vault | Vault ETH: held and owed, Vault queue |
| Details: deposits | Deposit indexer: blocks, Deposits |

| Choice | Reason |
|---|---|
| Dashboards and the data source as files, edits in the UI refused | What is shown is what is reviewed in the repository; a panel changed by hand would be lost and could not be reviewed. |
| Anonymous visitors are viewers | Anyone on this machine can open the dashboard without a password; nobody can change it without one. Grafana listens on 127.0.0.1 only. |
| Admin password created once by `obs-init.sh`, read by Grafana from a file | It is not in `compose.yaml`, an env file or `docker inspect`. The file is 644 because Grafana reads it as its own user inside the container; its folder is 700, so nobody else on this machine can reach it. `make obs-password` copies it to the clipboard without printing it. |
| Update checks, usage reporting, the news feed and Gravatar off | A local tool should not call out. |

`make obs-check` shows that all eleven targets are up and each of the nine jobs reports
series; that Grafana is healthy, the admin password works and the home page is this
dashboard; that an anonymous visitor reads every panel and cannot write; that every query
of every panel, closed rows included, returns data from Prometheus, names its chain and
shows over the 30 minutes on screen the same series as now; and it prints each tile with
its one number, unit and colour, worked out from the tile's thresholds in the file. An
empty panel fails the check, so a renamed metric is noticed at once. A stopped series
fails it too: the first overview checked only the value now, while the screen showed 30
minutes, and three tiles split into a live number and the last numbers of series left
behind by the label rename.

The first version of the batches tile read the batcher's own metric, the time of its
last confirmed transaction. When the batcher stops, that metric stops with it, the query
returns nothing and the tile shows no data instead of red. The tile now reads the safe
head from op-node, which keeps reporting while batches stop: it measures the effect,
not the batcher's report about itself.

## Alerts

Prometheus evaluates the rules in
[alerts.yml](../../infra/observability/alerts.yml) every 5 seconds and sends what fires
to Alertmanager v0.35.1, pinned by digest, at `http://127.0.0.1:9093`. What each alert
means and what to do is in [docs/runbooks.md](../runbooks.md).

| Alert | Tile | Fires when, for 1 minute |
|---|---|---|
| L2BlocksStalled | L2 makes blocks | Fewer than 25 blocks a minute |
| BatchesNotReachingL1 | Batches reach the L1 | 3 minutes or more of L2 not confirmed by the L1 |
| VerifierBehind | Verifier keeps up | 5 or more blocks behind |
| ProposalsStalled | Proposals are made | No proposal for 15 minutes |
| L1FinalityStalled | L1 finalizes | Finalized 160 or more blocks behind the head |
| OperatorFundsLow | Operators have funds | Batcher or proposer under 1 ETH |
| VaultInsolvent | Vault holds what it owes | At once: the vault holds less ETH than it owes |
| VaultWithdrawalQueued | Vault queue is empty | At once: a withdrawal waits in the queue |
| VaultPaused | Vault is open | At once: the vault is paused |
| VaultWatchStale | Vault is watched | No reading of the chain for a minute |
| DepositIndexerStale | Deposits are indexed | No step for a minute |
| DepositIndexerBehind | Indexer keeps up | 30 or more blocks behind the node's head |
| DepositFinalityViolation | Finality holds | At once: a block held final left the chain |
| LedgerOutOfBalance | Ledger balances | At once: the books differ from the journal |
| SuspenseUnresolved | Suspense explained | At once: a payment has waited in suspense a day or more; a ticket, not a page |
| TargetDown | none | A client does not answer the scrape |

| Choice | Reason |
|---|---|
| One rule per tile, the same query and red line | What is red on screen is what alerts, and the reverse. `make obs-check` reads both files and fails if a rule's query is not its tile's query compared with the tile's red line. |
| Held for 1 minute | One slow scrape or a late batch does not page anyone; a real stall still does within a minute of the red line. |
| The vault's alerts at once | They read the chain's state exactly, not a rate, so one reading is the truth. A queued withdrawal has only the queue delay (2 minutes on the devnet) to be checked and cancelled; holding it for a minute would spend half of it. `make vault-check` shows the queue and the pause alerts reach Alertmanager and clear. |
| TargetDown | A stopped client takes its metrics with it, and a query over missing metrics returns nothing, not red. Only `up == 0` sees that. |
| Every rule links a section of `docs/runbooks.md` | An alert without a known next step wakes someone up for nothing. `make obs-check` fails on a rule whose section is missing. |
| Two severities, page and ticket | A page is for what can lose money within the hour. SuspenseUnresolved is a ticket: the books are right, an explanation is owed, and it is held to a day, as suspense items are aged in reconciliation. |
| One receiver that keeps the alerts, nobody paged | The devnet has no on-call. In production the same rules route `severity=page` to the on-call rotation; only `alertmanager.yml` changes. |

`make obs-check` also shows that Prometheus loaded and evaluates every rule, sends to
Alertmanager, and that no alert fires on a healthy devnet. `make alert-drill` proves the
path end to end rather than only that the rules load. It
stops op-batcher, waits for BatchesNotReachingL1 to fire in Prometheus and arrive in
Alertmanager, checks that TargetDown fires for the batcher, then starts it again and
waits until both clear. The batcher is started again on any exit, and nothing is lost:
it posts every block since the safe head when it is back.

The first run of these checks stopped on a real stall. The computer had slept for about
three hours: Docker was paused, the L1 validator missed its slots, and the L1 finalized
block stayed at 946. The L2 finalized head stayed at 3605 with it, the proposer had no
new state to post (it kept retrying the last one, which the factory refused as
`GameAlreadyExists`), and the proposals tile was red with ProposalsStalled pending.
After wake the L1 finalized again and op-node took the new block at its next L1 poll.
Prometheus slept with the devnet, so nothing could fire during the sleep; in production
the monitoring runs apart from what it watches. The runbooks of ProposalsStalled and
L1FinalityStalled name this cause.
