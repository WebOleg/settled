# Design: withdrawals

How a customer's withdrawal leaves the vault exactly once: held in the
[ledger](ledger.md) from the moment it is accepted, paid by the vault under an id that
can be paid only once, and settled when the payment is final. The code is
[internal/withdrawals](../../internal/withdrawals/payer.go), its state is in
[0004_withdrawals.sql](../../internal/ledger/migrations/0004_withdrawals.sql), and every
rule below has a test in [payer_test.go](../../internal/withdrawals/payer_test.go).

## At most once, at least once

At most once is the vault's: it pays an id once and refuses it after
(`WithdrawalExists`, rule I3 of the [vault](vault.md)). The id is keccak256 of a domain,
the vault's address and the request's idempotency key, so a request sent again is the
same id, and an id is never shared between vaults.

At least once is the payer's. A payment can be lost between the database and the chain
in a few ways: a crash after signing and before sending, a transaction a node drops, a
reorganisation that takes a mined payment out of the chain. The payer does not keep its
own idea of what happened: at every step it reads what the vault records for the id
and acts on that alone.

| The vault has, for the id | The payer |
|---|---|
| Nothing | Sends the open transaction for it again, the same bytes, if there is one; otherwise checks the call against the vault without sending it and signs a new one |
| Queued | Waits for the release block, then pays it with `executeQueued` the same way |
| Paid or cancelled | Signs nothing; the withdrawal stays `onchain` until it is settled or cancelled |

## A request

`Submit` holds the amount on the customer's balance (a pending ledger transfer from the
customer to `vault:ETH`) and records the withdrawal, in one transaction. A request sent
again with the same key returns the withdrawal recorded; another request under that
key is refused, and so is one larger than the balance left after holds. Two identical
requests at once are one.

## A step of the payer

Each open withdrawal is worked on in one database transaction that holds its row; a
row another payer holds is skipped (`FOR UPDATE SKIP LOCKED`). In it the payer:

1. closes the withdrawal's transactions that no reorganisation can undo any more: mined
   in a final block, or whose nonce a final block has used;
2. reads the vault's record of the id and decides as in the table above;
3. before signing, runs the call against the vault without sending it: a refusal that
   no retry changes (a recipient that refuses ETH, a token not allowed) rejects the
   withdrawal and releases the hold; any other (the rate limit, a pause, the vault short
   of funds) is recorded in `last_error` and waited out;
4. takes the next nonce from one row per key, locked, so payers that share the key take
   turns, and records the signed transaction (an outbox) before anything is sent.

After the commit it sends what it recorded. A node that already has the transaction,
or has mined its nonce, says so, which is fine: the next step reads the receipt. A
transaction stays open until final, so if a reorganisation takes it out of the chain
it is sent again, the same bytes, and its nonce is never left empty for the ones after
it to wait on.

| Status | Meaning | Set by |
|---|---|---|
| `requested` | Held; the vault has nothing for the id | the request, or the payer after a reorganisation |
| `onchain` | The vault has the id, paid or queued, not yet final | the payer |
| `rejected` | The vault refuses it for good; the hold is released | the payer |
| `settled` | Paid and final; the hold is posted | `deposit_indexer` |
| `cancelled` | Cancelled by the guardian, and final; the hold is released | `deposit_indexer` |

A trigger allows only these moves, and nothing about a withdrawal but its status and
last error ever changes.

## Rules

| Id | Rule | Shown by |
|---|---|---|
| W1 Hold | A request holds its amount at once; one larger than the balance left is refused, and the same request twice is one withdrawal. | `TestSubmit_holdsTheBalanceOnce` |
| W2 Once | The vault pays each withdrawal once, and the payer signs no second transaction while one is in flight. | `TestPayer_paysOnceAndThenOnlyWatches` |
| W3 Crash | A transaction recorded and not sent is sent later, the same bytes. | `TestPayer_aCrashBeforeSendingSendsTheSameBytesLater` |
| W4 Dropped | A transaction a node lost is sent again. | `TestPayer_aDroppedTransactionIsSentAgain` |
| W5 Reorganisation | A payment a reorganisation removed is paid again, once, by the same transaction. | `TestPayer_aReorganisationIsPaidAgainOnce` |
| W6 Refusals | A refusal for good releases the hold; the rate limit is waited out. | `TestPayer_aRefusalForGoodReleasesTheHold`, `TestPayer_aRateLimitIsWaitedOut` |
| W7 Queue | A large withdrawal is paid with `executeQueued` from its release block, not before. | `TestPayer_aQueuedWithdrawalIsPaidAfterItsDelay` |
| W8 Payers at once | Four payers with one key, while blocks are mined: every withdrawal paid once, no nonce used twice or skipped. | `TestPayer_payersAtOnceWithOneKeyPayEachOnce` |

The tests run against a real PostgreSQL and a fake node that keeps a mempool, mines in
nonce order, can lose its mempool and reorganise, and applies the vault's rule that an
id is paid once.

## Roles

| Role | May |
|---|---|
| `withdrawal_requester` | Record a request and hold the balance (`ledger_writer`) |
| `withdrawal_payer` | Move a withdrawal between requested, onchain and rejected; record and close its transactions; take nonces; release a rejected hold (`ledger_writer`) |
| `deposit_indexer` | Settle or cancel a withdrawal once the chain has made it final |

## Known limits

| Limit | Why it is accepted here |
|---|---|
| A transaction is sent again with the fees it was signed with. | Fees on the devnet are flat. A rising base fee would call for replacing it with higher fees under the same nonce. |
| The operator key also signs for `make vault-check` on the devnet. | A payer takes the chain's nonce when it is ahead of its own, so a run of vault-check between two steps costs nothing; at the same moment, one transaction fails and is sent again. |
