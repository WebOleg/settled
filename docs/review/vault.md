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

## Manual review

Each area read against the code, with what shows it holds. Unit tests are in
[test/unit](../../contracts/test/unit), fuzz tests in
[VaultFuzz.t.sol](../../contracts/test/fuzz/VaultFuzz.t.sol), invariant tests in
[test/invariant](../../contracts/test/invariant), the Halmos proofs in
[VaultSymbolic.t.sol](../../contracts/test/symbolic/VaultSymbolic.t.sol); `make
vault-check` runs on the devnet.

| Area | What was checked | Shown by | Result |
|---|---|---|---|
| Reentrancy | Every function that moves value has `nonReentrant` first; state and events come before any external call; a deposit pulls the token last | `test_deposit_reentryIsBlocked`, `test_withdraw_reentryIsBlocked` (the guard's own `ReentrantCall`), forge lint | Holds |
| One payment per id | Any earlier use of an id, paid, queued or cancelled, rules it out | `test_withdraw_paysOnce`, `testFuzz_I3_I5_paidOrQueuedOnce`, `invariant_I3_paidOnce`, `check_I3_paidIdNeverPaysAgain`, `vault-check` | Holds |
| Solvency | No payment beyond what deposits account for; a queued payment checks again when paid | `testFuzz_I1_I2_neverPaysBeyondAccounted`, `invariant_I1_solvency`, `invariant_I2_accounting`, `check_I1_I2_neverBeyondAccounted`, `vault-check` to the wei | Holds |
| Rate limit | Payments at once over any span stay within capacity plus the refill; the queue does not draw on it; a new configuration does not refill it | `testFuzz_I4_rateLimitBound`, `invariant_I4_rateLimit`, `check_I4_paysWithinAvailable`, `test_setTokenConfig_doesNotRefillAndNewTokenStartsEmpty`, `vault-check` | Holds; finding F5 |
| Queue | Not paid before its release block, never after a cancel; cancelling still works while paused | `test_executeQueued_onlyAfterRelease`, `test_cancelQueued_byGuardianIsFinal`, `test_pause_stopsDepositsAndPayments`, `invariant_I5_queue`, `vault-check` | Holds; finding F2 |
| Roles | Operator pays, guardian pauses and cancels, the admin does the rest; no key holds two of them; the admin changes in two steps with a delay | `test_withdraw_onlyOperator`, `test_adminFunctions_onlyAdmin`, `test_roles_oneKeyOneRole`, `test_adminTransfer_twoStepsWithDelay`, `invariant_I7_roles`, `check_I7_onlyOperatorPays`, `vault-verify` | Holds; findings F1, F3 |
| Pause | Stops deposits and payments; only the admin, through the timelock, unpauses | `test_pause_guardianPausesOnlyAdminUnpauses`, `invariant_I6_pause`, `vault-check` | Holds; finding F3 |
| Initialization | The proxy is initialized in its own creation, so no one can do it first; the implementation refuses | `test_implementation_cannotBeInitialized`, `test_initialize_cannotRunTwice`, `vault-verify` | Holds |
| Upgrades | Only the admin can upgrade; state stays in the proxy; every field is where the test pins it, inside its namespace; an upgrade is refused before it is proposed if the new code would move a field | `test_upgrade_byAdminKeepsState`, `test_I10_layout`, `test_I10_namespaceIsErc7201`, `test_I10_nothingOutsideNamespaces`, the tests of `internal/storagelayout`, devnet upgrades with `vault-verify` | Holds; finding F6, fixed |
| Tokens | No return value handled; a fee on transfer refused in and out; only allowed tokens | `test_deposit_tokenWithoutReturnValue`, `test_deposit_rejectsFeeOnTransfer`, `test_deposit_rejectsBadInput` | Holds; finding F4 |
| ETH | No ETH without an account; a recipient that refuses leaves the id unused; no return data copied | `test_receive_rejectsPlainEther`, `test_withdraw_nativeToRejectingContractLeavesIdUnused` | Holds |
| Paying the vault itself | ETH to the vault is refused by its own `receive`; a token sent to itself leaves nothing, which the exact-amount check refuses | `test_withdraw_toTheVaultItselfIsRefused` | Holds |
| Funds not owed | Skim takes only what is held beyond what is owed, ETH forced in included | `test_skim_takesOnlyWhatIsNotOwed`, `test_skim_nativeForcedIn`, `testFuzz_I9_skimLeavesAccounted`, `invariant_I9_skim` | Holds |
| Front-running | Ids come from the ledger and only the operator can use them; deposits carry their account; admin changes are public in the timelock on purpose | Reading the code | Nothing to front-run |
| Time | The clock is the block number; delays and refills count blocks | `test_clock_isTheBlockNumber`, `vault-verify` (block time fixed by derivation) | Holds |

### Findings

| Id | Finding | Severity | Status |
|---|---|---|---|
| F1 | One key is the timelock's proposer, executor and canceller. Whoever steals it can propose any change, an upgrade included, and only that same key can cancel it: the delay gives time to see the change, but no one to stop it. | High | Open. |
| F2 | Nothing watches the queue: the guardian has the queue delay to cancel a large withdrawal it does not expect, but no alert tells it one was queued. | Medium | Open. |
| F3 | A stolen guardian key can pause again after every unpause and cancel every queued withdrawal. It cannot move funds. | Low | By design: the admin revokes the role through the timelock, so the damage lasts one timelock delay. |
| F4 | A token whose balance changes by itself breaks the books: a negative rebase leaves the vault owing more than it holds, and a fee switched on later makes its payments revert. | Medium if such a token is allowed | A rule for the admin: allow only plain ERC-20 tokens. Only ETH is allowed on the devnet. |
| F5 | The refill rounds down at each payment, losing less than one wei of allowance each time. | Informational | Accepted: the error only ever makes the limit stricter. |
| F6 | `vault-upgrade` checked the new code but not that its storage layout matched the deployed one. | Low | Fixed. The deployment record keeps the storage layout of the code the vault runs; `vault-upgrade` refuses, before it proposes anything, an implementation that moves, renames or retypes a recorded variable or puts a new one over its bytes ([internal/storagelayout](../../internal/storagelayout/storagelayout.go)); `vault-verify` checks the record against the build. |
