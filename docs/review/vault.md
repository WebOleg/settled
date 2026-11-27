# Review: vault

The findings on [Vault.sol](../../contracts/src/Vault.sol) and what was done about each.
The specification is [docs/design/vault.md](../design/vault.md). Every finding is
fixed in the code, or suppressed in the source on the line it concerns with the reason
next to it; `make check` fails on any lint finding left standing.

## forge lint

forge lint (Foundry v1.9.0) on `src/`.

| Lint | Where | Severity | Resolution |
|---|---|---|---|
| `non-reentrant-not-first` | `deposit`, `withdraw`, `executeQueued`, `skim` | Low | Fixed. `nonReentrant` is the first modifier, so the guard is taken before any other modifier runs. |
| `unsafe-typecast` | `releaseAt`, the rate limit's level and timestamp | Low | Fixed. Every narrowing goes through `SafeCast` and reverts rather than truncates. None could overflow (a timestamp fits a uint64, the level is at most the uint128 capacity), and now the compiler holds that, not an argument. |
| `reentrancy-events` | `deposit` | Low | Fixed. The deposit wrote its books and emitted `Deposited` after pulling the token. It now does both first and pulls the token last, reverting unless exactly the amount arrived, so a token that calls back finds the books already final. |
| `reentrancy-events` | every other event | None | Suppressed for the contract, with the reason at the top of the file. The lint flags every event, also in functions that make no external call (`cancelQueued`, `setTokenConfig`, `setWithdrawalDelay`); it does not see through the ERC-7201 storage accessor. Checked by hand: every event is emitted before any external call that can change state, and each function that makes one is guarded by `nonReentrant`. |
| `block-timestamp` | `executeQueued`, the release time | None | One time source, `_now()`, explains why block time can be trusted here; the comparison is suppressed on its line with that reason. On an OP Stack L2 the timestamp of a block is fixed by derivation, and `make vault-verify` checks that on the devnet. The case that remains, chain time catching up after the sequencer stops, is in the security table of the specification. |
