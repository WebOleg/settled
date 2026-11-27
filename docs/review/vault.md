# Review: vault

The findings on [Vault.sol](../../contracts/src/Vault.sol) and what was done about each.
The specification is [docs/design/vault.md](../design/vault.md). Every finding is
fixed in the code, or in the tool where the tool was wrong; findings in libraries this
repository does not change, and the one no code can meet, are reviewed one by one under
Slither. No check is switched off: `make check` fails on any lint finding, any compiler
warning and any Slither finding not reviewed here.

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

`layout at` did not help with these findings: they came from OpenZeppelin's accessors.
It is used now for another reason, under Slither below.

## Compiler warnings

The contracts build with no warning filter: `ignored_error_codes = []` turns off the
ones forge hides by default, and the libraries are not exempt. Two warnings were
hidden before.

| Warning | Where | Resolution |
|---|---|---|
| 2394, transient storage | OpenZeppelin's `TransientSlot`, used by `ReentrancyGuardTransient` | Fixed. The vault has its own guard on a `transient` state variable: the same transient storage, managed by the compiler instead of inline assembly, so there is nothing to warn about. OpenZeppelin's storage-based `ReentrancyGuard` would also avoid it, but it is deprecated. Tests check that a token calling back into `deposit` or `withdraw` gets `ReentrantCall`, the guard's own error, not some other refusal. |
| 3860, initcode over 48 KB | the unit test contract | Fixed. The unit tests are split into one contract per topic on a shared base, and the upgrade test deploys the next version from its artifact, so no test contract embeds two copies of the vault. |

## Slither

Slither 0.11.6 on `src/` and everything it imports. Every finding is fixed in the code
or recorded in [contracts/slither.db.json](../../contracts/slither.db.json), Slither's
triage file, under the hash Slither gives it, which covers the finding's source lines:
change a line and the finding is new. `make check` fails on any finding not recorded and
on a recorded one that no longer occurs, so the record is exactly what Slither reports.
Filtering the libraries out by path was not used: Slither drops a finding as soon as one
of its elements lies in a filtered path, so a finding in the vault that names a library
function would have gone unseen.

Found in the vault, and fixed:

| Detector | Where | Resolution |
|---|---|---|
| `incorrect-equality` | `skim` | Fixed. `amount == 0` on a value derived from a balance; `skim` now refuses when the balance is at most what is owed, then subtracts. |
| `assembly` | `_send` | Fixed. ETH is sent with OpenZeppelin's `LowLevelCall.callNoReturn`: no return data is copied, as before, without assembly of our own. |
| `assembly` | the storage accessor | Fixed. The state is plain variables that the compiler places in the ERC-7201 namespace (`layout at erc7201("settled.storage.Vault")`), at the same slots as before; `VaultLayout.t.sol` checks them. |
| `naming-convention` | `CLOCK_MODE()`, `NATIVE()` in `IVault` | Fixed. `CLOCK_MODE` is a public constant, which still meets ERC-6372; `NATIVE` is a constant of the vault (the ERC-7528 address), no longer a function of the interface. |

Recorded, 86 findings. One touches the vault; the others lie in OpenZeppelin v5.7.0,
pinned and audited, which this repository does not change.

| Detector | Severity | Count | Where | Why it is not a defect |
|---|---|---|---|---|
| `pragma` | Informational | 1 | every file | The pragmas differ (ours `0.8.39`, OpenZeppelin's `^0.8.20` to `^0.8.24`, and older ones in interfaces). What the detector asks for, one compiler, holds: `foundry.toml` builds everything with solc 0.8.39. |
| `solc-version` | Informational | 7 | OpenZeppelin pragmas | The ranges admit old compilers with known bugs; the build uses 0.8.39, which has none. |
| `incorrect-exp` | High | 1 | `Math.mulDiv` | `^` is meant as exclusive or: it seeds the modular inverse in a Newton iteration. |
| `divide-before-multiply` | Medium | 9 | `Math.mulDiv`, `Math.invMod` | Deliberate steps of exact 512-bit division and of the inverse computation. |
| `msg-value-in-nonpayable` | High | 1 | `ERC1967Utils._checkNonPayable` | It is reached from `upgradeToAndCall`, which is payable; it refuses ETH sent with an upgrade that calls nothing. |
| `unused-return` | Medium | 3 | `ERC1967Utils.upgradeToAndCall`, `upgradeBeaconToAndCall`, `Time.get` | The call's revert is what matters and is passed on; `Time.get` reads one field of three. |
| `timestamp` | Low | 6 | `AccessControlDefaultAdminRulesUpgradeable`, `Time` | The admin handover counts seconds by design; its delay is days in production and the timelock guards it too. |
| `assembly` | Informational | 39 | OpenZeppelin storage accessors, `LowLevelCall`, `Math` and others | Audited assembly of the library. |
| `naming-convention` | Informational | 15 | OpenZeppelin initializers, `IERC6372.CLOCK_MODE` | Names fixed by the library and by the standard. |
| `unindexed-event-address` | Informational | 3 | `Paused`, `Unpaused`, `AdminChanged` | Events fixed by the library and by ERC-1967. |
| `too-many-digits` | Informational | 1 | `Math.log2` | A bit mask written out in full. |
