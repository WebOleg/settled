# Bridge service

The ledger's PostgreSQL, the deposit indexer, which keeps the ledger in step with the
vault on the L2 (deposits held when seen, credited once final, voided if a
reorganisation removes them first; payments settled once final), and the withdrawal
payer, which pays the ledger's withdrawals from the vault through the withdrawal
signer, the only one that may sign with its key, which is in kms-dev and never leaves
it. Why it works this way is in
[docs/design/deposits.md](../../docs/design/deposits.md),
[docs/design/withdrawals.md](../../docs/design/withdrawals.md),
[docs/design/signer.md](../../docs/design/signer.md),
[docs/design/kms-dev.md](../../docs/design/kms-dev.md) and
[docs/design/ledger.md](../../docs/design/ledger.md).

It needs the running L2 (`make l2-up`); the indexer follows the vault once one is
deployed (`make vault-deploy`) and waits until then.

## Commands

| Command | What it does |
|---|---|
| `make bridge-up` | Makes the passwords and kms-dev's access keys in `.devnet/bridge` on first run and the signer's TLS certificates in `.devnet/bridge/tls`, starts PostgreSQL and waits until it is healthy, and kms-dev, migrates the schema and sets the logins below with `ledger-migrate`, makes the payer's key in kms-dev and, once a vault is deployed, gives it the operator role (`make vault-payer`), then builds and starts the indexer, the signer and the payer |
| `make bridge-check` | Reconciles the ledger with the chain, read independently as an auditor that can only read: every deposit up to the indexer's last final block posted with its amount and account and nothing else posted, none of them still held, each customer credited what the chain deposited for it and debited what the chain paid for its settled withdrawals, every payment applied (a settled withdrawal, or suspense when the ledger never requested it) and nothing else, suspense holding those payments less the ones explained under four eyes, the books agreeing with the journal, and what the ledger holds for the vault equal to the vault's `accounted(ETH)`, to the wei |
| `make withdrawal-check` | Sends one real withdrawal through the payer and follows it: a deposit of 0.1 ETH for a new account credited once final; 0.04 ETH requested and held at once; paid by the payer from the vault and still held; settled once the payment is final, and not before; the books agreeing with the journal. About half an hour |
| `make suspense-list` | Lists the payments in suspense not yet explained, oldest first, with any proposal waiting for a second operator; how to explain one is in [the runbook](../../docs/runbooks.md#suspenseunresolved) |
| `make deposit-check` | Deposits 0.1 ETH of the devnet user for a new account and follows it through the ledger: held, not spendable, while the verifier's finalized block is below it; credited once it is final, and not before; the books agreeing with the journal. Prints how long each step took; about a quarter of an hour |
| `make bridge-drill` | Stops PostgreSQL, so the indexer runs but every step fails; shows `DepositIndexerStale` fire and reach Alertmanager, starts PostgreSQL, shows it clear |
| `make bridge-logs` | Follows the logs of the five services |
| `make bridge-down` | Stops the five services and keeps the ledger and kms-dev's keys |
| `make bridge-reset` | Stops the five services and deletes the ledger, kms-dev's keys, the passwords, the access keys and the certificates; the next `make bridge-up` makes a new payer's key and takes the role from the one lost |
| `make vault-payer-rotate` | Replaces the payer's key: makes a new one in kms-dev, gives it the operator role through the timelock, takes the role from the old one and empties it into the new account, and starts the signer again on the new key |

`make up` starts the bridge after the L2, `make down`, `make l2-down` and `make l1-down`
stop it first, and `make reset` and `make l2-reset` delete the ledger with the chain it
was kept for.

## Services

| Service | Image | Reaches |
|---|---|---|
| `postgres` | PostgreSQL 18.7, pinned by digest; the checks run the same one ([scripts/lib/postgres.sh](../../scripts/lib/postgres.sh)) | `127.0.0.1:15432` |
| `deposit-indexer` | Built from this repository ([Dockerfile](../images/deposit-indexer/Dockerfile)) on pinned bases; no shell, unprivileged | the verifier's execution JSON-RPC, the deployment record read-only; Prometheus scrapes its metrics over the bridge's network |
| `withdrawal-payer` | Built from this repository ([Dockerfile](../images/withdrawal-payer/Dockerfile)) the same way | the sequencer's execution JSON-RPC, the signer over TLS with its client certificate, the deployment record read-only; it holds no key; Prometheus scrapes its metrics over the bridge's network |
| `withdrawal-signer` | Built from this repository ([Dockerfile](../images/withdrawal-signer/Dockerfile)) the same way | no node; PostgreSQL as `signer`; kms-dev as `withdrawal-signer`, which may only read the payer's public key and sign with it; its access key, server certificate and the CA as Docker secrets; its API on the bridge's network only; Prometheus scrapes its metrics port |
| `kms-dev` | Built from this repository ([Dockerfile](../images/kms-dev/Dockerfile)) the same way | nothing; its keys in the volume `kms-keys`; its API on the bridge's network and `127.0.0.1:14599`, for the admin; Prometheus scrapes its metrics port |

## Logins

| Login | Holds | Used by |
|---|---|---|
| `admin` | The owner of the schema | `ledger-migrate` |
| `indexer` | `deposit_indexer`: writes through the ledger's functions, its own blocks and deposits | the indexer |
| `payer` | `withdrawal_payer`: records and closes its transactions, takes nonces, moves a withdrawal between requested, onchain and rejected | the payer |
| `signer` | `withdrawal_signer`: reads the withdrawals, writes `signer_log` | the signer |
| `requester` | `withdrawal_requester`: holds a balance and records a withdrawal | `withdrawal-check` |
| `auditor` | `ledger_reader`: reads every table, writes nothing | `bridge-check`, `deposit-check`, `suspense-list` |
| `ops_a`, `ops_b` | `suspense_operator`: proposes, approves or rejects the resolution of a payment in suspense, through its functions only | two operators, one login each (`cmd/suspense`) |

Each password is 48 hex characters from `openssl rand`, in a file in `.devnet/bridge`,
a folder readable by its owner only; services read it as a Docker secret, commands from
the file. The access keys of kms-dev's three principals, its admin's, the signer's and
key rotation's, are there too, and the signer's CA and certificates in `tls`, by `cmd/signer-tls`. The
payer's key is in kms-dev only ([kms-dev](../../docs/design/kms-dev.md)). None is printed, in a flag, or in `docker inspect`.
