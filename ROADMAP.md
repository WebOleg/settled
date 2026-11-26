# Roadmap

The only file in the repository that describes work not done yet. Each stage ends
when its "done when" line is true and is shown by something in the repository.

| Stage | Goal | Done when | Status |
|---|---|---|---|
| 0. Foundation | Repository, rules, tooling | Rules are written and the local checks run with one command | Done |
| 1. Devnet | A local L1 and an OP Stack L2 with a verifier node | `make up` starts the chain and the verifier derives the same state from L1 alone | In progress |
| 2. Observability | Metrics, dashboards and alerts for the chain | Every alert has a documented meaning, a threshold and a runbook | Not started |
| 3. Contracts | A small set of audited-style Solidity contracts | Unit, fuzz and invariant tests pass; a written review lists every finding | Not started |
| 4. Bridge and ledger | A Go service that moves funds between L1 and L2 | Deposits credit only at finality, reorgs reverse in the ledger, a withdrawal is paid once under retries and races | Not started |
| 5. Kubernetes and CI | The same system on local Kubernetes, tested in CI | CI starts the devnet and runs the end-to-end tests on every push | Not started |
| 6. Incident drills | Planned failures with runbooks and post-mortems | Each drill has a runbook and a post-mortem, including a transaction forced in through L1 | Not started |
| 7. Upgrades | Node and contract upgrades, a scheduled hard fork | An upgrade and a hard fork activation are done on the devnet with a written checklist | Not started |
| 8. Fault proofs | A dispute game on the devnet | An invalid output is challenged and loses | Not started |
| 9. Notes | Rust clients and ZK proving, read and explained | Written notes link to the upstream code they describe | Not started |
