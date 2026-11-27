# Design: the withdrawal signer

The payer decides what to pay; it does not hold the key that pays. The key that holds
the vault's operator role lives in the withdrawal signer
([internal/signer](../../internal/signer/signer.go),
[cmd/withdrawal-signer](../../cmd/withdrawal-signer/main.go)), which signs for the payer
only what its policy and the ledger allow, the way a custodian's policy engine stands
between a hot wallet and its key. A payer, a host or a database row that is not what it
should be can then ask for anything, and get nothing signed that the ledger does not
hold.

## What it signs

| Rule | The transaction |
|---|---|
| `type` | Is an EIP-1559 transaction |
| `chain` | Is for the chain the signer serves, from the deployment record |
| `destination` | Calls the vault, from the deployment record |
| `value` | Carries no value |
| `access list` | Has no access list |
| `gas` | Carries at most `-max-gas` |
| `fee cap` | Offers at most `-max-fee-cap-gwei`, its tip at most its fee cap |
| `method` | Calls `withdraw` or `executeQueued`, encoded canonically, and nothing else |
| `ledger` | Names a withdrawal the ledger holds open for this vault; `withdraw` pays exactly its token, recipient and amount |
| `velocity` | With what `withdraw` was signed for in the last `-window`, per token, stays within `-limit-milli-eth`; each withdrawal counts once, however often it is signed again |
| `client` | Comes from the payer's client certificate |

The signer reads the ledger itself, as a login that holds only `withdrawal_signer`: it
may read `withdrawals` and write its own record, nothing else. The nonce is the
payer's; the signer does not choose it, and a nonce used twice only replaces.

## Every decision recorded first

Each request, signed or refused, is one row in `signer_log`
([0009_signer.sql](../../internal/ledger/migrations/0009_signer.sql)): when, the client,
the decision and the rule, what was asked (chain, nonce, gas, fees, destination, method,
withdrawal, token, recipient, amount) and, when signed, the transaction's hash. The row
is written in the same database transaction as the decision, and the signature leaves
only after it commits, so there is no signature without a record. The table is
append-only, for its owner too. Decisions are made one at a time across every signer of
the key (an advisory lock), so the velocity limit is read and spent atomically.

## How the payer reaches it

Over HTTPS with TLS 1.3, both ways: the signer's certificate is checked by the payer
against the signer's CA, and the payer's by the signer, which signs only for the
certificate name it allows (`withdrawal-payer`). The API is two calls: the account it
signs for, and sign. On the devnet the CA and both certificates are made by
[cmd/signer-tls](../../cmd/signer-tls/main.go) in `.devnet/bridge/tls`, a folder only its
owner reads, again before they end; in production they come from the organisation's
PKI.

The payer checks what comes back: the same chain, nonce, fees, gas, destination, value
and data it asked for, signed for the operator's account. A refusal is waited out: the
payer records why in the withdrawal's `last_error`, takes no nonce, and asks again at
its next step.

## The key

The signer reaches the key through two calls, its address and the signature of a hash
(`signer.Key`). On the devnet the key is a file read by the signer's container only, as
a Docker secret; the payer's container has none. A key in an HSM or a KMS, which never
leaves it, goes behind the same two calls: AWS KMS, for one, holds secp256k1 keys
(`ECC_SECG_P256K1`), signs a digest with `ECDSA_SHA_256` and returns the public key,
from which the account is derived.

## Shown by

| Id | Rule | Shown by |
|---|---|---|
| G1 Signs what the ledger holds | A withdraw that pays what the ledger holds, and an executeQueued of an open withdrawal, are signed and recorded with their hash. | `TestSign_whatTheLedgerHoldsIsSignedAndRecorded` |
| G2 Refuses the rest | Each rule refuses what breaks it, and the refusal is recorded. | `TestSign_whatThePolicyForbidsIsRefusedAndRecorded` |
| G3 Velocity | The limit counts each withdrawal once, a replacement adds nothing, and the window moves on. | `TestSign_theVelocityLimitCountsEachWithdrawalOnce` |
| G4 At once | Ten requests at once under a limit for six: six signed. | `TestSign_signersAtOnceSpendTheLimitOnce` |
| G5 The record | `signer_log` is append-only, for its owner too; the signer can change nothing else. | `TestSignerLog_isAppendOnlyAndTheSignerChangesNothingElse` |
| G6 Only the payer | Over HTTPS, the payer's certificate signs; another name from the same CA is refused and recorded; no certificate, no TLS. | `TestHandler_onlyTheAllowedClientSigns` |
| G7 End to end | On anvil every transaction of the payer is signed by the signer over TLS and recorded; another recipient, amount, id, contract and a plain transfer are refused; a payer the velocity limit refuses waits and takes no nonce. | `make check-bridge` ([cmd/payer-e2e](../../cmd/payer-e2e/main.go)) |

The payer's side is shown in [withdrawals](withdrawals.md): a refusal is waited out and
takes no nonce, and a transaction other than the one asked for is not recorded.

## Known limits

| Limit | Why it is accepted here |
|---|---|
| The key is a file in the signer's container. | Only the signer reads it, and it signs only what the ledger holds. A KMS or HSM backend is the other implementation of `signer.Key`; it is not built, because no KMS runs on this machine without an account: LocalStack's KMS signs with secp256k1 keys, and since March 2026 its image needs an auth token. |
| One signer serves the key; a second would share the database and the lock. | The lock and the record already allow several; the devnet runs one. |
