# Design: withdrawals

How a customer's withdrawal leaves the vault exactly once: held in the
[ledger](ledger.md) from the moment it is accepted, paid by the vault under an id that
can be paid only once, and settled when the payment is final. The code is
[internal/withdrawals](../../internal/withdrawals/payer.go), its state is in
[0004_withdrawals.sql](../../internal/ledger/migrations/0004_withdrawals.sql) and
[0007_fee_replacement.sql](../../internal/ledger/migrations/0007_fee_replacement.sql),
and every rule below has a test in [payer_test.go](../../internal/withdrawals/payer_test.go).
A payment that leaves the vault without the ledger is explained under four eyes
([payments in suspense](#payments-in-suspense)).

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
| Paid or cancelled | Signs nothing; the indexer settles or cancels it once final ([deposits](deposits.md#payments-and-cancellations)) |

## A request

`Submit` holds the amount on the customer's balance (a pending ledger transfer from the
customer to `vault:ETH`) and records the withdrawal, in one transaction. A request sent
again with the same key returns the withdrawal recorded; another request under that
key is refused, and so is one larger than the balance left after holds. Two identical
requests at once are one.

## A step of the payer

Each open withdrawal is worked on in one database transaction that holds its row; a
row another payer holds is skipped (`FOR UPDATE SKIP LOCKED`). In it the payer:

1. closes the withdrawal's transactions that no reorganisation can undo any more: a
   version mined in a final block, or a nonce a final block has used; the same is done
   for a withdrawal the indexer has settled, cancelled or rejected meanwhile, whose
   transaction in flight, if any, is sent again so its nonce is not left empty;
2. reads the vault's record of the id and decides as in the table above;
3. before signing, runs the call against the vault without sending it: a refusal that
   no retry changes (a recipient that refuses ETH, a token not allowed) rejects the
   withdrawal and releases the hold; any other (the rate limit, a pause, the vault short
   of funds) is recorded in `last_error` and waited out;
4. waits while the network asks a fee cap above `MaxFeeCap`, then takes the next nonce
   from one row per key, locked, so payers that share the key take turns, and records
   the signed transaction (an outbox) before anything is sent.

After the commit it sends what it recorded. A node that already has the transaction,
or has mined its nonce, says so, which is fine: the next step reads the receipt. A
transaction stays open until final, so if a reorganisation takes it out of the chain
it is sent again, and its nonce is never left empty for the ones after it to wait on.

## Fee replacement

A transaction signed when fees were low is not mined once the network asks more, and
every later nonce of the key waits behind it. After `BumpAfter` unmined, the payer
replaces it, as custodians do: the same call, gas and nonce, with the tip and fee cap
each raised by an eighth, or to the network's if those are higher. Nodes accept a
replacement only for at least a tenth more; the eighth leaves room for the base fee to
rise a block meanwhile. A replacement never offers more than `MaxFeeCap`: with no room
left for a tenth more, the transaction is left as it is, sent again when due, and the
reason is recorded in `last_error`.

Each version is recorded before it is sent, like the first. Any of them may be the one
mined, the first included if a replacement never reached the node, so the slot (sender
and nonce) stays open until a version is final, and then all of its versions close.
Only the latest version is sent again. The database holds the outbox to this: a
version records its fees and call; a replacement is of the latest version of an open
slot, for the same withdrawal, call, sender and nonce, a tenth higher in both fees;
one first version per slot; nothing recorded changes, and a closed transaction stays
closed.

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
| W9 Replacement | A transaction the network's fees left behind is replaced under the same nonce after `BumpAfter`; the replacement is mined and the withdrawal paid once, one nonce used. | `TestPayer_aStuckTransactionIsReplacedAndPaidOnce` |
| W10 Fee cap | No replacement goes above `MaxFeeCap`, and nothing is signed, and no nonce taken, while the network asks more. | `TestPayer_aReplacementStopsAtMaxFeeCap`, `TestPayer_aNetworkAboveMaxFeeCapIsWaitedOut` |
| W11 Any version | The first version mined after a replacement was recorded closes the slot; nothing is paid or signed again. | `TestPayer_theFirstVersionMinedAfterAReplacementClosesTheSlot` |
| W12 Outbox | The database refuses a version without fees, a second first version of a slot, a replacement less than a tenth higher, of another call or nonce, a second replacement of one version, and reopening a closed one. | `TestOutbox_theDatabaseRefusesWhatIsNotAReplacement` |
| W13 Closed after settlement | A withdrawal the indexer settled before the payer looked again still has its transactions closed once final, and nothing is signed for it. | `TestPayer_closesTheTransactionsOfASettledWithdrawal`, `make check-bridge` |

The tests run against a real PostgreSQL and a fake node that keeps a mempool, mines in
nonce order, leaves unmined what tips below its minimum, replaces a transaction only
for a tenth more, can lose its mempool and reorganise, and applies the vault's rule
that an id is paid once.

`make check-bridge` then runs the payer against a real node, anvil, with the vault
deployed as on the devnet and the indexer crediting and settling
([cmd/payer-e2e](../../cmd/payer-e2e/main.go)): a withdrawal paid once; a send that fails
after the transaction was recorded, sent again with the same bytes; a payment
`anvil_reorg` removed, paid again by the same transaction; two payers with one key
paying six at once, nonces contiguous; a base fee raised far above what a sent
transaction offers, the transaction replaced and only the replacement mined; a large
withdrawal queued and paid with `executeQueued` after the delay. Once final every
withdrawal is settled, every transaction closed, nothing is in suspense, and the
ledger equals the vault on chain.

## The service

[cmd/withdrawal-payer](../../cmd/withdrawal-payer/main.go) runs the steps every two
seconds, reads the vault from the deployment record at each, and signs with a key read
from a file. It serves Prometheus metrics: what each step signed, sent again, replaced,
left at the fee cap, rejected, reopened and closed; the withdrawals open and how long
the oldest with nothing on chain has waited; the transactions open; what its key holds
for gas. Its image ([Dockerfile](../../infra/images/withdrawal-payer/Dockerfile)) is
built like the indexer's: pinned bases, no shell, an unprivileged user.

## Payments in suspense

A payment the ledger never requested, made with the operator's key outside the payer,
is posted from `vault:ETH` to `suspense:ETH` once final
([deposits](deposits.md#payments-and-cancellations)). The ledger still equals the vault;
what is missing is whose money it was. It is explained under four eyes
([internal/suspense](../../internal/suspense/suspense.go),
[0008_suspense_resolution.sql](../../internal/ledger/migrations/0008_suspense_resolution.sql)):

1. One operator proposes, with a reason: `writeoff`, the money is lost to the exchange
   and posted to `writeoff:<asset>`; or `customer`, it was owed to a customer and paid
   outside the ledger, so their balance pays it.
2. A second operator approves it, which moves the whole payment out of suspense to that
   account in the same transaction, or rejects it with why, after which it may be
   proposed again.

Each operator connects as a login of their own, holding `suspense_operator`, and the
database tells them apart by it (`session_user`): the one who proposed cannot approve,
whatever the client does. Operators act only through the functions; they cannot write
a table or move money otherwise. Proposals and decisions are kept with who made them,
when and why, and never change after. A payment is resolved whole and once.
`make suspense-list` shows what waits; `cmd/suspense` proposes, approves and rejects.
The indexer reports how long the oldest has waited, and SuspenseUnresolved opens a
ticket at a day ([runbook](../runbooks.md#suspenseunresolved)). `make bridge-check`
holds suspense to the payments posted there less those approved, each approval to the
transfer it made.

| Id | Rule | Shown by |
|---|---|---|
| S1 Four eyes | Money leaves suspense only on the approval of an operator other than the one who proposed. | `TestResolve_aSecondOperatorApprovesAWriteOff`, `make check-bridge` |
| S2 Customer | A payment charged to a customer is paid by their balance only if it covers it; a refused approval leaves the proposal waiting. | `TestResolve_aCustomerPaysOnlyWhatTheirBalanceCovers` |
| S3 Once | A payment has one proposal open or approved; a rejection says why and lets it be proposed again; an approved one is final. | `TestResolve_aPaymentIsResolvedOnce` |
| S4 Only operators | A login without `suspense_operator` cannot propose; an operator cannot write the tables or call the ledger's functions. | `TestResolve_onlyOperatorsAndOnlyThroughTheFunctions` |

## Roles

| Role | May |
|---|---|
| `withdrawal_requester` | Record a request and hold the balance (`ledger_writer`) |
| `withdrawal_payer` | Move a withdrawal between requested, onchain and rejected; record and close its transactions; take nonces; release a rejected hold (`ledger_writer`) |
| `deposit_indexer` | Settle or cancel a withdrawal once the chain has made it final; read what waits in suspense |
| `suspense_operator` | Propose, approve or reject the resolution of a payment in suspense, through the functions only; one login per person |

## Known limits

| Limit | Why it is accepted here |
|---|---|
| A payment in suspense is resolved whole, to one account. | A payment that is part a customer's and part a loss is rare; resolving in parts is not built, and such a payment stays in suspense, visible and alerted on, until it is. |
| The operator key also signs for `make vault-check` on the devnet. | A payer takes the chain's nonce when it is ahead of its own, so a run of vault-check between two steps costs nothing; at the same moment, one transaction fails and is sent again. |
