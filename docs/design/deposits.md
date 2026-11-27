# Design: deposit indexer

How a deposit to the vault on the L2 reaches a customer's balance in the
[ledger](ledger.md): seen at once, credited only once final, undone if a reorganisation
removes it first. The code is [internal/deposits](../../internal/deposits/indexer.go),
its state is in [0002_deposits.sql](../../internal/ledger/migrations/0002_deposits.sql),
and every rule below has a test in
[indexer_test.go](../../internal/deposits/indexer_test.go).

## Finality

The indexer reads the chain from a node, the verifier in the devnet, and uses two of
its heads: the latest block, which the sequencer made and can still change, and the
finalized block, derived from L1 data that the L1 has finalized, which cannot change
without breaking the L1. It is the rule rollups give exchanges, for example
[Arbitrum's integration checklist](https://docs.arbitrum.io/launch-arbitrum-chain/integrations/exchange-integration-checklist):
a deposit is shown as soon as it is seen and credited only once its block is final on
the parent chain.

| Where the deposit's block is | Deposit | Ledger |
|---|---|---|
| Above the finalized block | `pending` | A pending transfer from `vault:ETH` to the customer: shown, not spendable |
| At or below the finalized block | `posted` | The transfer posted: the customer can spend it |
| Gone from the chain before it was final | `voided` | The transfer voided: nothing credited |

A deposit is keyed by the block it is in and its log index, not by the vault's
deposit id: after a reorganisation the same id can name another deposit, and the same
transaction can come back in another block. That one is voided in the old block and
held again in the new one, so it is credited once.

## Payments and cancellations

The indexer reads the vault's `WithdrawalPaid` and `WithdrawalCancelled` the same way,
keyed by block and log index, held as pending until final and voided if their block
leaves the chain first. Nothing reaches the ledger before they are final.

| A final event | The ledger |
|---|---|
| A payment of a withdrawal the ledger holds ([withdrawals](withdrawals.md)), to its recipient and for its amount | The hold posted; the withdrawal `settled` |
| Any other payment: an id the ledger never requested, or a different recipient or amount | Posted from `vault:ETH` to `suspense:ETH`: money that left the vault without the ledger, kept visible until it is explained under four eyes ([withdrawals](withdrawals.md#payments-in-suspense)), as a bank keeps unmatched items |
| A cancellation of a withdrawal the ledger holds | The hold released; the withdrawal `cancelled` |

So what the ledger holds for the vault always follows the chain, whatever paid it.
On the devnet, `make vault-check` pays from the vault directly with the operator's key;
those payments are in suspense.

## Streams and backfill

The indexer reads two streams of events, deposits and withdrawals, each from a block of
its own (`indexer_streams`), so a version that reads a new kind of event joins a ledger
that is already running without rebuilding it. A stream starts at the vault's
deployment block on a new ledger. A stream added later is read live from the block
after the tip it is added at, which its migration records; the blocks below are read
once more for that stream alone, a backfill: a range of final blocks at a time, each in
one transaction with the stream's position, and the blocks not yet final once they are.
Every event is applied once: below its stream's start by the backfill, from it on live.
A stream without a start on a ledger that has read blocks is refused, not read from
the beginning, which would count again what was read.

The database holds the rule that makes this safe while two versions run side by side,
as in a rolling deploy (migration 0006): each stored block lists the streams read in
it, and a block at or above a stream's start without that stream is refused. An indexer of a version that
does not read the stream, still running after the migration, cannot store a block
whose events of the stream nobody would then read; it fails and the new one goes on.

`bridge-check` compares withdrawal events only up to where the backfill has reached
while it runs, and the ledger with the vault once it is done.

## A step

1. Read the node's latest and finalized blocks.
2. Compare the highest stored block, or the stored block at the node's head if the
   chain got shorter, with the node's block at that height. If they differ, walk down
   the stored blocks to the highest one still in the chain, the fork, and in one
   transaction void every pending deposit, payment and cancellation above it and
   forget the blocks above it.
3. Read final blocks the indexer has not read yet a range at a time
   (`eth_getLogs` over block numbers is safe there), applying their events at once.
4. Read blocks not yet final one at a time: each must name the stored tip as its
   parent, and its logs are read by block hash (EIP-234), so they belong to that
   block even if the chain moves meanwhile. A block that does not sit on the tip ends
   the step; the next one unwinds.
5. If the stored block at the finalized height is the node's finalized block, mark
   every stored block up to it final, post every pending deposit up to it, then apply
   every pending payment and cancellation up to it in chain order.

Every write is one transaction that also takes an advisory lock for the vault and
checks that the stored tip is still the one the step built on. A crash between two
transactions leaves a state a step goes on from, and a second indexer on the same
database can only repeat work, never count a deposit twice: each block and each
deposit is unique in the database, and each ledger transfer has its own key.

## Rules

| Id | Rule | Shown by |
|---|---|---|
| D1 Finality | A deposit is credited only once the node's finalized block reaches it. | `TestStep_creditsADepositOnlyOnceFinal` |
| D2 Reorganisation | A held deposit whose block leaves the chain is voided, including when the chain gets shorter. | `TestStep_aReorganisationVoidsAHeldDeposit`, `TestStep_aShorterChainVoidsWhatItLost` |
| D3 Once | A transaction included again in another block is credited once. Two indexers at once, through reorganisations, credit every deposit exactly once. | `TestStep_aDepositMovedToAnotherBlockIsCreditedOnce`, `TestStep_twoIndexersAtOnceCountEachDepositOnce` |
| D4 Final is final | A final block is never unwound. If the node's chain no longer has one, the indexer stops with `ErrFinalityViolation` and changes nothing: credits were made on a chain that is gone, which a person must look at. | `TestStep_aFinalBlockLeavingTheChainStopsTheIndexer`, a trigger on `indexed_blocks` |
| D5 Atomic | The chain as read and the ledger change together; an error from the node leaves both as they were. | `TestStep_aNodeErrorChangesNothing` |
| D6 History | A deposit only moves from pending to posted or voided; no row is deleted. | A trigger on `deposits` |
| D7 Settlement | A final payment of a withdrawal the ledger holds posts its hold; a payment not yet final does nothing, and one that leaves the chain first settles nothing. | `TestStep_aFinalPaymentSettlesItsWithdrawal`, `TestStep_aPaymentThatLeftTheChainSettlesNothing` |
| D8 Suspense | A final payment the ledger never requested is posted to suspense, so the ledger still holds what the vault holds. | `TestStep_aPaymentNobodyRequestedGoesToSuspense` |
| D9 Cancellation | A final cancellation releases the hold. | `TestStep_aFinalCancellationReleasesTheHold` |
| D10 Backfill | A stream added after blocks were read gets the final blocks below its start read once for it alone, the ones not yet final once they are, and every event is applied once; a stream with no start is refused. | `TestStep_aStreamAddedLaterIsBackfilledOnce`, `TestStep_aStreamWithoutAStartIsRefused` |
| D11 Fence | A block at or above a stream's start cannot be stored without that stream, so an older indexer still running after the migration is refused. | `TestStep_anOlderIndexerCannotWriteAfterTheMigration`, a trigger on `indexed_blocks` |
| D12 Chain order | Catching up over final blocks, what was held below them is applied first, so a payment never comes before the deposit that paid for it. | `TestStep_eventsApplyInChainOrderWhenCatchingUp` |

The indexer runs as `deposit_indexer`, which holds `ledger_writer` and may insert
blocks, deposits and withdrawal events, delete blocks that are not final, change a
deposit's or an event's status and a block's final flag, and settle or cancel a
withdrawal, nothing else.

## End to end

`make check-bridge` ([cmd/bridge-e2e](../../cmd/bridge-e2e/main.go)), part of
`make check`, runs the indexer against a real node that can be made to reorganise:
anvil from the pinned Foundry image, whose finalized block trails its head by 64
blocks, and a throwaway PostgreSQL, migrated by `ledger-migrate` with the indexer's
login, as on the devnet. It deploys the vault behind its proxy and sends real
deposits:

1. two deposits are held while their blocks are not final, and credited once the
   finalized block reaches them;
2. `anvil_reorg` drops the block of a third, held deposit: it is voided and nothing of
   it is credited;
3. a fourth deposit's signed transaction is put back by `anvil_reorg` into a new block:
   voided in the old block, held in the new one, credited once;
4. the operator pays half an ETH from the vault without the ledger: once final it is
   in suspense; after the run, `make check-bridge` has two operators explain it with
   `cmd/suspense`, and the one who proposed cannot approve;
5. the books agree with the journal, and what the ledger holds for the vault equals
   `accounted(ETH)` of the vault on chain at the finalized block, to the wei.

## The command

[cmd/deposit-indexer](../../cmd/deposit-indexer/main.go) steps every 2 s, finds the
vault in the deployment record, read again at every step, and serves its metrics on
`/metrics`: the node's head and finalized block, its own tip and lag, deposits seen,
posted and voided, reorganisations, failed steps, deposits still held, and the result
of `ledger_check()`, run every 30 s. After `ErrFinalityViolation` it writes nothing
more and reports `deposit_indexer_finality_violation 1`. The database password comes
from a file, never a flag. [cmd/ledger-migrate](../../cmd/ledger-migrate/main.go)
migrates the schema as its owner and makes each service's login, holding only the role
it needs.

On the devnet the indexer and its PostgreSQL run as the bridge service
([infra/bridge](../../infra/bridge/README.md)), and `make bridge-check`
([cmd/bridge-check](../../cmd/bridge-check/main.go)) reconciles the ledger with the
chain from outside the indexer: it reads the vault's events itself, as a login that
can only read, and compares them with the books up to the indexer's last final block,
deposit by deposit and to the wei. `make deposit-check`
([cmd/deposit-check](../../cmd/deposit-check/main.go)) sends a real deposit and follows
it: held at once, credited only when the verifier's finalized block reaches it. The
indexer's tiles, alerts and runbooks are in
[observability.md](observability.md#alerts).

## Known limits

| Limit | Why it is accepted here |
|---|---|
| Below the last final block only the last block of each range read is stored. | Final blocks are never compared again; above it every block is kept. |
| Only ETH is allowed in the vault on the devnet. | Any token's deposits are indexed under an asset named by its address; whether the vault takes the token is the admin's rule ([vault review](../review/vault.md), F4). |
