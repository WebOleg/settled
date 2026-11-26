# settled

A self-run OP Stack L2 with a bridge that moves money safely, operated the way a
production network is operated.

The project has two parts. The first runs the chain: a local L1 and an OP Stack L2
that are deployed, monitored, upgraded and broken on purpose, with a runbook and a
post-mortem for every failure. The second moves money across it: a Go service that
indexes both chains and records every movement in a double-entry ledger. Deposits are
credited only at finality, reorgs are undone by reversing entries, and each withdrawal
is paid once.

Everything runs on one machine and needs no cloud account.

## Status

Stage 1 is in progress. The local L1 runs. The L2 does not run yet; its operator keys
and their funding exist. This file
describes only what exists and grows with the code.

The rules the code follows are in [ENGINEERING.md](ENGINEERING.md). The plan is in
[ROADMAP.md](ROADMAP.md).

## Local L1

```sh
make l1-up
make l1-check
make l1-blob-check
```

A proof-of-stake L1 with geth and Lighthouse, started from its own genesis. Requires
Docker, curl, jq and Go. Services, endpoints and files are described in
[infra/devnet/l1/README.md](infra/devnet/l1/README.md); the reasons behind the setup are
in [docs/design/devnet.md](docs/design/devnet.md).

## L2 operator keys

```sh
make l2-keys
make l2-fund
```

One key per operator role, kept in `.devnet/l2/keys` and readable only by its owner.
`l2-fund` tops up the accounts that pay gas on the L1. The roles and the rules for the
keys are in [docs/design/devnet.md](docs/design/devnet.md#l2-operator-roles-and-keys).

## Checks

```sh
make check
```

Requires gitleaks, shellcheck and Go. What each check does is in
[ENGINEERING.md](ENGINEERING.md#local-checks).

## License

MIT. See [LICENSE](LICENSE).
