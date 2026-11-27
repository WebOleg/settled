# Review: vault

The findings on [Vault.sol](../../contracts/src/Vault.sol) and what was done about each.
The specification is [docs/design/vault.md](../design/vault.md). Every finding is
fixed in the code or, where the finding came from the tool, in the tool; nothing is
suppressed, and `make check` fails on any lint finding.

## forge lint

forge lint on `src/`, first with Foundry v1.9.0, now with v1.10.1, which reports
nothing.

| Lint | Where | Severity | Resolution |
|---|---|---|---|
| `non-reentrant-not-first` | `deposit`, `withdraw`, `executeQueued`, `skim` | Low | Fixed. `nonReentrant` is the first modifier, so the guard is taken before any other modifier runs. |
| `unsafe-typecast` | `releaseAt`, the rate limit's level and time point | Low | Fixed. Every narrowing goes through `SafeCast` and reverts rather than truncates. None could overflow, and now the compiler holds that, not an argument. |
| `reentrancy-events` | `deposit` | Low | Fixed. The deposit wrote its books and emitted `Deposited` after pulling the token. It now does both first and pulls the token last, reverting unless exactly the amount arrived, so a token that calls back finds the books already final. |
| `reentrancy-events` | every other event | None | A defect of forge lint v1.9.0, fixed by moving to v1.10.1. v1.9.0 counted every inline assembly block as an external call, and `onlyRole` and `whenNotPaused` reach OpenZeppelin's ERC-7201 storage accessors, which are assembly; so every event after them was flagged, also in functions that make no external call. A probe contract showed it: a storage write and an event pass, the same behind `onlyRole` or `whenNotPaused` is flagged. v1.10.0 counts assembly only when it can reach a call or a contract creation. The vault's bytecode is the same under both versions. |
| `block-timestamp` | `executeQueued`, the release time | Low | Fixed. The vault counted time in block timestamps, which whoever produces a block chooses within the chain's rules. Its clock is now the block number (ERC-6372), which no one chooses; delays and refills are counted in blocks. On the OP Stack a block comes every 2 s, fixed by derivation, and `make vault-verify` checks that, so the delays keep their durations. Hiding the comparison behind a library function would have silenced the lint without changing what the vault depends on. |

`layout at`, which places plain state variables at the ERC-7201 slot, was tried to take
the vault's own storage accessor out of the way. The compiler refuses to let another
contract inherit from one with a custom layout, which the upgrade tests rely on, and
the findings came from OpenZeppelin's accessors in any case.
