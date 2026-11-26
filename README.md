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

Stage 0 is done: the repository, its rules and the local checks. No chain or service
runs yet. This file describes only what exists and grows with the code.

The rules the code follows are in [ENGINEERING.md](ENGINEERING.md). The plan is in
[ROADMAP.md](ROADMAP.md).

## Checks

```sh
make check
```

Requires gitleaks and shellcheck. What each check does is in
[ENGINEERING.md](ENGINEERING.md#local-checks).

## License

MIT. See [LICENSE](LICENSE).
