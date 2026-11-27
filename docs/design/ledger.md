# Design: ledger

The exchange's books: who is owed what, and what the exchange holds, in wei. This file
is its specification; the schema is
[0001_ledger.sql](../../internal/ledger/migrations/0001_ledger.sql), the Go package
[internal/ledger](../../internal/ledger/ledger.go), and every rule below has a test in
[ledger_test.go](../../internal/ledger/ledger_test.go).

## Model

Double entry, as banks keep books, in the shape of
[TigerBeetle](https://docs.tigerbeetle.com/coding/two-phase-transfers/)'s accounts and
transfers. Every transfer moves one amount of one asset from a debit account to a
credit account, so all debits always equal all credits. A balance is never written by
hand: it is the sum of the transfers, kept on the account as four totals (debits
pending and posted, credits pending and posted) in the same transaction as the
transfer.

| Account | Limit | Balance |
|---|---|---|
| `vault:ETH`, what the exchange holds | credits must not exceed debits | debits minus credits |
| `customer:<id>:ETH`, what the exchange owes a customer | debits must not exceed credits | credits minus debits |

A transfer is one of four kinds:

| Kind | Effect |
|---|---|
| `single` | Posts at once. |
| `pending` | Reserves the amount: on hold on the debit side, not yet spendable on the credit side. |
| `post` | Completes a pending transfer, with its accounts and its amount. |
| `void` | Cancels a pending transfer and releases the hold. |

Deposits and withdrawals of the vault map onto them so: a deposit is pending from the
moment it is seen on the L2 and posted once its block is final, or voided if a
reorganisation removes it. A withdrawal holds the customer's balance while it is paid
and is posted once the payment is final. A pending debit
counts against the limit, a pending credit does not: money on hold is not there to
spend, and money not yet final is not there yet.

## Rules

The database enforces them, so no caller, a bug in the service included, can break
them.

| Id | Rule | Enforced by |
|---|---|---|
| L1 Double entry | All debits equal all credits, pending and posted. | Every transfer moves one amount between two accounts; `ledger_check()` |
| L2 Limits | A customer's account never goes below zero, nor what the exchange holds. | `CHECK` constraints on the account's totals |
| L3 Idempotency | The same key is one transfer. The same request again returns it; another request under the same key is refused. | `UNIQUE (key)`, and `ledger_transfer` compares every field |
| L4 One resolution | A pending transfer is posted or voided at most once. | `UNIQUE (pending_id)` |
| L5 Append only | A transfer is never changed or deleted, even by the owner of the schema; a correction is a new transfer. | Triggers on `UPDATE`, `DELETE` and `TRUNCATE` |
| L6 One asset | Both accounts of a transfer hold the same asset. | `ledger_transfer` |
| L7 One way in | The service writes only through `ledger_account` and `ledger_transfer`; it cannot change a balance or the journal by hand. | It holds `ledger_writer`: `SELECT` on the tables, `EXECUTE` on the functions, which run as the owner |
| L8 Exact amounts | Amounts are unsigned 256-bit integers, as on chain; never floats. | The `wei` domain, `numeric(78, 0)` |

`ledger_check()` recomputes every account's four totals from the journal and returns
every difference, and a difference between all debits and all credits; none means the
books are right. Every test ends with it.

## Concurrency

A transfer locks its two accounts in id order before it writes anything, so two
transfers never wait on each other in a cycle; the lock is `FOR NO KEY UPDATE`, which
the balance updates take anyway. Two requests with the same key at once are one
transfer: the second waits for the first and then returns it. A post and a void of
the same pending transfer at once: exactly one wins, the other is refused as already
resolved. The tests run many workers on the same accounts, each request sent twice,
with the race detector.

## Migrations

The schema is created by numbered migrations embedded in the binary
([migrate.go](../../internal/ledger/migrate.go)), applied in order, each in its own
transaction, under an advisory lock so two processes never apply one at once. Each
applied migration is recorded with its sha256; a migration changed after it was applied
stops the next run, since the database would no longer be the schema the code
describes. Migrations run as the owner of the schema; the service connects as a role
that holds only `ledger_writer`.

## Tests

`make check-go` starts a throwaway PostgreSQL in the pinned image on a random local
port, with a random password that is never printed, and runs the tests against it;
each test gets a database of its own. Without that server the tests fail rather than
skip.

## Known limits

| Limit | Why it is accepted here |
|---|---|
| Every deposit and withdrawal locks `vault:ETH`, so they are serialized on it. | A devnet's volume is far below what one row lock takes. At scale the hot account is split into several and summed. |
| A pending transfer is posted in full, never in part. | Deposits and withdrawals settle in full. |
| Pending transfers do not expire by themselves. | Each one is resolved by the chain: finality, a reorganisation or the payment. |
