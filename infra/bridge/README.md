# Bridge service

The ledger's PostgreSQL and the deposit indexer, which keeps the ledger in step with
the vault's deposits on the L2: held when seen, credited once final, voided if a
reorganisation removes them first. Why it works this way is in
[docs/design/deposits.md](../../docs/design/deposits.md) and
[docs/design/ledger.md](../../docs/design/ledger.md).

It needs the running L2 (`make l2-up`); the indexer follows the vault once one is
deployed (`make vault-deploy`) and waits until then.

## Commands

| Command | What it does |
|---|---|
| `make bridge-up` | Makes the passwords in `.devnet/bridge` on first run, starts PostgreSQL and waits until it is healthy, migrates the schema and sets the indexer's and the auditor's logins with `ledger-migrate`, then builds and starts the indexer |
| `make bridge-check` | Reconciles the ledger with the chain, read independently as an auditor that can only read: every deposit up to the indexer's last final block posted with its amount and account and nothing else posted, none of them still held, each customer credited what the chain deposited, every payment applied (a settled withdrawal, or suspense when the ledger never requested it) and nothing else, the books agreeing with the journal, and what the ledger holds for the vault equal to the vault's `accounted(ETH)`, to the wei |
| `make deposit-check` | Deposits 0.1 ETH of the devnet user for a new account and follows it through the ledger: held, not spendable, while the verifier's finalized block is below it; credited once it is final, and not before; the books agreeing with the journal. Prints how long each step took; about a quarter of an hour |
| `make bridge-drill` | Stops PostgreSQL, so the indexer runs but every step fails; shows `DepositIndexerStale` fire and reach Alertmanager, starts PostgreSQL, shows it clear |
| `make bridge-logs` | Follows the logs of both services |
| `make bridge-down` | Stops both services and keeps the ledger |
| `make bridge-reset` | Stops both services and deletes the ledger and the passwords |

`make up` starts the bridge after the L2, `make down`, `make l2-down` and `make l1-down`
stop it first, and `make reset` and `make l2-reset` delete the ledger with the chain it
was kept for.

## Services

| Service | Image | Reaches |
|---|---|---|
| `postgres` | PostgreSQL 18.7, pinned by digest; the checks run the same one ([scripts/lib/postgres.sh](../../scripts/lib/postgres.sh)) | `127.0.0.1:15432` |
| `deposit-indexer` | Built from this repository ([Dockerfile](../images/deposit-indexer/Dockerfile)) on pinned bases; no shell, unprivileged | the verifier's execution JSON-RPC, the deployment record read-only; Prometheus scrapes its metrics over the bridge's network |

## Logins

| Login | Holds | Used by |
|---|---|---|
| `admin` | The owner of the schema | `ledger-migrate` |
| `indexer` | `deposit_indexer`: writes through the ledger's functions, its own blocks and deposits | the indexer |
| `auditor` | `ledger_reader`: reads every table, writes nothing | `bridge-check`, `deposit-check` |

Each password is 48 hex characters from `openssl rand`, in a file in `.devnet/bridge`,
a folder readable by its owner only; services read it as a Docker secret, commands from
the file. None is printed, in a flag, or in `docker inspect`.
