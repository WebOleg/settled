# Runbooks

One section per alert in
[infra/observability/alerts.yml](../infra/observability/alerts.yml), headed by the
alert's name; `make obs-check` fails if a rule has no section here. Each says what the
alert means, what to look at and what to do. The commands are for this devnet.

Logs of one service: `docker compose -f infra/devnet/l2/compose.yaml logs --tail 100
<service>` (or `l1/compose.yaml`). The dashboard is at `http://127.0.0.1:3000`, the
alerts at `http://127.0.0.1:9093`, and `make status` shows everything at once.

## L2BlocksStalled

The sequencer makes fewer than 25 L2 blocks a minute; normally 30, one every 2 s. New
transactions are not included.

Look at:
- `make status`: are `op-node` and `op-reth` running, does the unsafe head move.
- Logs of `op-node` and `op-reth`: engine API errors between them.
- Is the L1 advancing (`make l1-check`)? The sequencer stops when its L1 origin falls
  too far behind.

Do:
- A stopped service: `make l2-up` starts it with its data.
- The L1 stalled: see [L1FinalityStalled](#l1finalitystalled).

## BatchesNotReachingL1

Three minutes or more of L2 blocks are not confirmed by the L1: the unsafe head minus the
safe head, at 2 s a block. Normally under one minute. Until the L1 confirms them, these
blocks are not safe, and everything that waits for safety waits.

Look at:
- `make status`: is `op-batcher` running. TargetDown for `op-batcher` fires with it when
  the batcher is down.
- Dashboard, Details: batcher: blocks waiting to be batched grow, confirmed batches per
  minute drop to zero, the blob base fee jumps.
- Logs of `op-batcher`: failed or underpriced transactions, insufficient funds.

Do:
- A stopped batcher: `make l2-up`. It posts every block since the safe head; nothing is
  lost.
- No funds: see [OperatorFundsLow](#operatorfundslow).
- The L1 does not include its transactions: see [L1FinalityStalled](#l1finalitystalled).

`make alert-drill` stops the batcher on purpose and shows this alert fire and clear.

## VerifierBehind

The verifier is 5 or more blocks behind the sequencer's unsafe head. The independent
copy of the chain is late: its RPC and the states the proposer reads from it are old.

Look at:
- `make l2-check`: the verifier's peer is connected and its blocks match the sequencer's.
- Dashboard, Details: L2: op-node peers at zero.
- Logs of `verifier-op-node`.

Do:
- Peer lost: `docker compose -f infra/devnet/l2/compose.yaml restart verifier-op-node`.
  It catches up from the peer, and derives every safe block from the L1 in any case.
- Its blocks differ from the sequencer's: this is an incident, not a delay. Trust
  neither until the cause is known.

## ProposalsStalled

The proposer has not posted a state to the L1 for 15 minutes; normally about every 6.
Withdrawals cannot be proven against newer states and wait. No funds are at risk.

Look at:
- `make status`: is `op-proposer` running, its balance.
- The proposer posts only finalized L2 states. If BatchesNotReachingL1 or
  L1FinalityStalled fires, finality has stopped and so have proposals: fix that first.
- Logs of `op-proposer`. `make l2-games` checks the games posted so far.
- `failed to estimate gas ... reason: 0x014f6fe5` repeated every 2 s is
  `GameAlreadyExists(bytes32)`: the finalized state has not moved, so the proposer keeps
  trying to post the game it already posted. No transaction is sent and nothing is
  spent; it stops when finality moves again.

Do:
- A stopped proposer: `make l2-up`.
- No funds: see [OperatorFundsLow](#operatorfundslow).

## L1FinalityStalled

The L1 finalized block is 160 or more blocks (5 epochs) behind its head; normally 64 to
96. Nothing on the L2 becomes finalized, so proposals and withdrawals stop.

Look at:
- `make l1-check`.
- Dashboard, Details: L1: validator duties. This devnet has one validator with all the
  stake; when it stops attesting, finality stops.
- Logs of `beacon` and `validator`.

On this devnet the usual cause is the computer sleeping: Docker pauses, the validator
misses its slots and finality stops. After wake it resumes within a few epochs, and
op-node picks up the new finalized block at its next L1 poll, every 6.4 minutes; the
L2 finalized head, then proposals, follow.

Do:
- A stopped L1 service: `make l1-up` starts it with its data.
- On a public L1 finality is outside the operator's control. The L2 keeps running on safe
  blocks, and whatever waits for finality waits.

## OperatorFundsLow

The batcher or the proposer holds less than 1 ETH on the L1. When it runs out, batches
or proposals stop.

Look at:
- `make status`: the balances.
- Dashboard, Details: proposer and funds: how fast they fall.

Do:
- `make l2-fund` tops both up. In production the top-up comes from a treasury account
  with a limit, and the threshold is set from the daily spend.

## VaultInsolvent

The vault holds less ETH than its deposits account for, compared to the wei. Its own
checks make this impossible for ETH; if it fires, something outside the code's
assumptions happened, and every further payment risks paying one user with another's
funds.

Look at:
- Dashboard, Details: vault: when held fell below owed.
- The vault's events since then (`WithdrawalPaid`, `Skimmed`) and its implementation: an
  upgrade that was not expected.

Do:
- The guardian pauses the vault at once; nothing is paid while it is paused.
- Find the cause before anything is unpaused; the admin unpauses only through the
  timelock.

## VaultWithdrawalQueued

A large withdrawal waits in the vault's queue. It can be paid from its release block
on, 60 blocks (2 minutes) after it was queued on the devnet; until then the guardian can
cancel it. A withdrawal the ledger did not ask for means the operator key is not safe.

Look at:
- Dashboard, Details: vault: how many and how much.
- The `WithdrawalQueued` event: its id, recipient and amount, and the ledger's entry for
  that id.

Do:
- It matches the ledger: nothing; the operator pays it after its release block and the
  alert clears.
- It does not: the guardian cancels it (`cancelQueued(id)`) and pauses the vault, and
  the operator key is replaced through the timelock.

## VaultPaused

The guardian has paused the vault: deposits and payments stop until the admin unpauses
it through the timelock.

Look at:
- Who paused and why: the guardian's `Paused` event and whoever holds the guardian key.

Do:
- A deliberate pause: nothing until its cause is resolved; then the admin unpauses
  through the timelock and the alert clears.
- A pause no one asked for: the guardian key is not safe; the admin revokes its role
  through the timelock.

## VaultWatchStale

The vault exporter has not read the chain for a minute, so the other vault alerts see an
old state and could miss a queued withdrawal.

Look at:
- `docker compose -f infra/observability/compose.yaml logs --tail 50 vault-exporter`:
  it logs each new error once.
- `make status`: is the verifier's op-reth running.

Do:
- The verifier is down: `make l2-up`.
- The exporter is down: `make obs-up` starts it.

## DepositIndexerStale

The deposit indexer has not completed a step for a minute. No new deposit is held,
none is credited at finality and none voided: customers wait, and a reorganisation is
not undone until it runs again.

Look at:
- `make bridge-logs`: the indexer logs each new error once, with what it was doing.
- `make status`: are the verifier's op-reth and the bridge's PostgreSQL running.

Do:
- The verifier is down: `make l2-up`.
- PostgreSQL or the indexer is down: `make bridge-up`. The indexer goes on from the
  last block it stored; nothing is counted twice.

A stopped indexer takes its metrics with it, and this alert with them: TargetDown
fires for it instead. `make bridge-drill` shows this alert for a database that is down.

## DepositIndexerBehind

The indexer is 30 or more blocks, a minute, behind the node's head. Deposits made in
that time are not yet held, so customers do not see them.

Look at:
- Dashboard, Details: deposits: is the indexed line moving toward the head, or flat.
- `make bridge-logs`: errors from the node or the database.

Do:
- Moving: it is catching up, after a restart or a long stop; it reads 300 blocks a step.
- Flat: as for DepositIndexerStale.

## DepositFinalityViolation

The node's chain no longer has a block the indexer held final. Deposits were credited
on a chain that is gone, and customers may have spent them. The indexer has stopped
writing and will not start again by itself.

Look at:
- `make bridge-logs`: the block, the hash the indexer stored and the hash the node has.
- `make status` and the verifier: is it on the same chain as the sequencer, and is its
  finalized head the one the L1 holds. A verifier rebuilt from scratch, or pointed at
  another L1, reports another chain.

Do:
- Stop crediting: the guardian pauses the vault, so nothing is paid on these balances.
- Find which credited deposits are not on the node's chain: compare `deposits` (status
  `posted`) with the chain, or run `make bridge-check`, which names the first.
- Correct the ledger with new transfers, never by editing it; then restart the indexer
  (`make bridge-down`, `make bridge-up`) only once the node is right.

## LedgerOutOfBalance

A balance in the ledger differs from the sum of its transfers, or all debits differ
from all credits. The ledger's functions make this impossible; if it fires, something
wrote to the tables around them.

Look at:
- `make bridge-logs`: the indexer logs each difference `ledger_check()` reports.
- Who can write: only the schema owner can change `accounts` by hand; the services'
  logins hold only their roles.

Do:
- Stop paying from the ledger until it is understood.
- Recompute from the journal, which nobody can change: `ledger_check()` names each
  account and its totals. Correct with transfers, never by editing balances.

## SuspenseUnresolved

A payment left the vault without the ledger, was posted to `suspense:ETH` once final,
and has waited a day or more without an approved explanation. The ledger still equals
the vault, but nobody has said whose money it was. It is a ticket, not a page: nothing
is at risk in the next hour, and it must not wait for weeks.

Look at:
- `make suspense-list`: each payment not yet explained, its amount, recipient, block,
  transaction and how long it has waited, and any proposal waiting for a second
  operator.
- The transaction on chain and who holds the key that sent it: the vault's operator,
  or someone acting with it.

Do:
- Find out what the payment was, with the recipient's owner if they are a customer.
- One operator proposes, with a reason, with their own login:
  `go run ./cmd/suspense -db 'postgres://<login>@127.0.0.1:15432/ledger?sslmode=disable' -db-password-file <file> propose -event N -writeoff -reason '...'`
  for money lost to the exchange, or `-customer customer:<account>:ETH` for a customer
  paid outside the ledger, whose balance then pays it.
- A second operator checks it and runs `approve -id N -note '...'`, or `reject -id N
  -note '...'` with why. The database refuses an approval by the operator who proposed,
  and moves the money only on approval. On the devnet the two operators are `ops_a` and
  `ops_b`, with passwords in `.devnet/bridge/ops-a.txt` and `ops-b.txt`.
- A payment nobody can explain is a security incident: the vault's operator key may be
  out of the exchange's control. Pause the vault (VaultPaused) before anything else.

## TargetDown

A client of the devnet has not answered Prometheus for a minute. Its own metrics are
gone, so the tiles that read them show no data rather than red; this alert is what
notices. The `job` and `node` labels name the client.

Look at:
- `make status`: which service is not running.
- Its logs: why it stopped, or why it does not start.

Do:
- `make l1-up` or `make l2-up` starts it with its data.
- It fails again on start: the logs say why; fix that before restarting it again.
