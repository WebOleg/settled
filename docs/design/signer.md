# Design: the withdrawal signer

The payer decides what to pay; it does not hold the key that pays. The key that holds
the vault's operator role is in a KMS, and only the withdrawal signer
([internal/signer](../../internal/signer/signer.go),
[cmd/withdrawal-signer](../../cmd/withdrawal-signer/main.go)) may sign with it, for the
payer and only what its policy and the ledger allow, the way a custodian's policy
engine stands between a hot wallet and its key. A payer, a host or a database row that
is not what it should be can then ask for anything, and get nothing signed that the
ledger does not hold.

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

The payer reads the account the signer signs for at each step, so a rotated key is
followed with its own nonces. It checks what comes back: the same chain, nonce, fees,
gas, destination, value and data it asked for, signed for that account. A refusal is waited out: the
payer records why in the withdrawal's `last_error`, takes no nonce, and asks again at
its next step.

## The key

The signer reaches the key through two calls, its address and the signature of a hash
(`signer.Key`). The key is in a KMS that speaks the AWS KMS API, and never leaves it
(`signer.KMSKey`, [kms.go](../../internal/signer/kms.go)): an `ECC_SECG_P256K1` key for
`SIGN_VERIFY`, reached with the AWS SDK for Go as a principal that may only read the
public key of `alias/payer` and sign a digest with it. The account is derived from the
public key; the KMS's DER signature becomes Ethereum's r, s and v, with s in the lower
half of the curve's order. In production the KMS is AWS KMS; on the devnet it is
kms-dev ([kms-dev](kms-dev.md)), with the same client and its own endpoint and
credentials. Neither the payer's container nor the signer's holds a key. With
`-key-file`, a key in a file takes its place, for runs without a KMS.

The signer signs only as the payer the deployment record names: it starts once the
record names its key's account, which `make vault-payer` writes when that account holds
the operator role.

## Shown by

| Id | Rule | Shown by |
|---|---|---|
| G1 Signs what the ledger holds | A withdraw that pays what the ledger holds, and an executeQueued of an open withdrawal, are signed and recorded with their hash. | `TestSign_whatTheLedgerHoldsIsSignedAndRecorded` |
| G2 Refuses the rest | Each rule refuses what breaks it, and the refusal is recorded. | `TestSign_whatThePolicyForbidsIsRefusedAndRecorded` |
| G3 Velocity | The limit counts each withdrawal once, a replacement adds nothing, and the window moves on. | `TestSign_theVelocityLimitCountsEachWithdrawalOnce` |
| G4 At once | Ten requests at once under a limit for six: six signed. | `TestSign_signersAtOnceSpendTheLimitOnce` |
| G5 The record | `signer_log` is append-only, for its owner too; the signer can change nothing else. | `TestSignerLog_isAppendOnlyAndTheSignerChangesNothingElse` |
| G6 Only the payer | Over HTTPS, the payer's certificate signs; another name from the same CA is refused and recorded; no certificate, no TLS. | `TestHandler_onlyTheAllowedClientSigns` |
| G7 End to end | On anvil every transaction of the payer is signed by the signer over TLS, with the operator's key in kms-dev, and recorded; another recipient, amount, id, contract and a plain transfer are refused; a payer the velocity limit refuses waits and takes no nonce. | `make check-bridge` ([cmd/payer-e2e](../../cmd/payer-e2e/main.go)) |
| G8 Only a secp256k1 signing key | A KMS key of another spec or usage, or a signature from another key, is refused. | `TestKMSAnswers_anythingButASecp256k1KeyAndItsSignatureIsRefused` |

The payer's side is shown in [withdrawals](withdrawals.md): a refusal is waited out and
takes no nonce, and a transaction other than the one asked for is not recorded.

## Known limits

| Limit | Why it is accepted here |
|---|---|
| The signer has not run against AWS KMS itself. | That needs an AWS account; on the devnet it runs against kms-dev with the same SDK client ([kms-dev](kms-dev.md#known-limits)). |
| One signer serves the key; a second would share the database and the lock. | The lock and the record already allow several; the devnet runs one. |
