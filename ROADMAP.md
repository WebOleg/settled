# Roadmap

The only file in the repository that describes work not done yet. Each stage ends
when its "done when" line is true and is shown by something in the repository.

| Stage | Goal | Done when | Status |
|---|---|---|---|
| 0. Foundation | Repository, rules, tooling | Rules are written and the local checks run with one command | Done |
| 1. Devnet | A local L1 and an OP Stack L2 with a verifier node | `make up` starts the chain and the verifier derives the same state from L1 alone | Done |
| 2. Output proposals | op-proposer posts the L2 state to the L1, and every proposal is recomputed independently | A withdrawal started on the L2 is proven and finalized on the L1 against a game the proposer created | Done |
| 3. Observability | Metrics, dashboards and alerts for the chain | Every alert has a documented meaning, a threshold and a runbook | Done |
| 4. Contracts | A small set of audited-style Solidity contracts | Unit, fuzz and invariant tests pass; a written review lists every finding | Done |
| 5. Bridge and ledger | A Go service that moves funds between L1 and L2 | Deposits credit only at finality, reorgs reverse in the ledger, a withdrawal is paid once under retries and races | In progress |
| 6. Kubernetes and end-to-end CI | The same system on local Kubernetes, tested end to end in CI | CI starts the devnet and runs the end-to-end tests on every push | Not started |
| 7. Incident drills | Planned failures with runbooks and post-mortems | Each drill has a runbook and a post-mortem, including a verifier rebuilt from the L1 with peer-to-peer off, a wrong proposal blacklisted by the guardian, and a transaction forced in through the L1 | Not started |
| 8. Upgrades | Node and contract upgrades, a scheduled hard fork | An upgrade and a hard fork activation are done on the devnet with a written checklist | Not started |
| 9. Fault proofs | A dispute game on the devnet under attack | An invalid output is challenged and loses | Not started |
| 10. Notes | Rust clients and ZK proving, read and explained | Written notes link to the upstream code they describe | Not started |

Output proposals come before the bridge: a withdrawal can only be paid on the L1 against
an output root that a dispute game has settled, so the bridge has nothing to finalize
without them. The static checks run in CI from stage 1 on; stage 6 adds the devnet to
CI.
