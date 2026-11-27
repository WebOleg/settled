# Contracts

The Solidity contracts of the exchange on the L2. What they do and the invariants they
keep are in [docs/design/vault.md](../docs/design/vault.md); the review and what was done
about each finding in [docs/review/vault.md](../docs/review/vault.md).

## Commands

| Command | What it does |
|---|---|
| `make check-contracts` | Checks formatting, builds without compiler warnings, checks that every source file built an artifact, runs the tests, compares the gas of the unit tests with `.gas-snapshot`, runs forge lint, the symbolic tests with Halmos and Slither; part of `make check` |
| `make vault-deploy` | Deploys the vault to the devnet L2 behind a timelock and a proxy, once per L2 ([docs/design/vault.md](../docs/design/vault.md#deployment)) |
| `make vault-verify` | Checks the deployed vault against this repository: code byte for byte, roles, delays, configuration |
| `make vault-upgrade` | Upgrades the vault to the code the repository builds, through the timelock; does nothing when it already runs that code |
| `make vault-check` | Runs the rules on the devnet with real transactions from each role's key: deposit, payment at once, a refused duplicate, the queue, the pause and the unpause through the timelock, every wei accounted for, and the queue and pause alerts firing and clearing ([docs/design/vault.md](../docs/design/vault.md#the-rules-on-the-devnet)) |
| `make vault-reset` | Forgets the deployment, keeps the keys, for a new L2 |
| `make contracts-snapshot` | Records the gas of the unit tests in `.gas-snapshot`. A change in gas shows in review as a change to that file. |
| `make contracts-slither-record` | Records Slither's findings in `slither.db.json`, after each new one is reviewed in docs/review/vault.md. A change shows in review as a change to that file. |

`FOUNDRY_PROFILE=ci` runs the fuzz and invariant tests deeper, as CI does
([foundry.toml](foundry.toml)). Forge runs in the pinned Foundry image, as the calling user, with its home folder on
`.cache/foundry` (where solc is kept after the first download). Nothing is installed on
the host.

## Toolchain

| Tool | Version | Pinned by |
|---|---|---|
| Foundry | v1.10.1 | image digest in the [Makefile](../Makefile) |
| solc | 0.8.39 | [foundry.toml](foundry.toml); downloaded and checked by forge |
| OpenZeppelin Contracts | v5.7.0 | git submodule at the tag's commit |
| OpenZeppelin Contracts Upgradeable | v5.7.0 | git submodule at the tag's commit |
| forge-std | v1.17.0 | git submodule at the tag's commit |
| Halmos | 0.3.3, with z3 4.12.6 | [infra/images/halmos](../infra/images/halmos): bases by digest, Python packages by sha256 |
| Slither | 0.11.6 | [infra/images/slither](../infra/images/slither): bases by digest, Python packages by sha256 for amd64 and arm64 |

| Choice | Reason |
|---|---|
| Foundry v1.10.1 | forge lint in v1.9.0 counts every inline assembly block as an external call, so it flags every event behind OpenZeppelin's storage accessors ([docs/review/vault.md](../docs/review/vault.md)); v1.10.0 fixed that, but its arm64 image ships `forge`, `cast` and `chisel` as 434 KB files that print nothing and exit 0. In v1.10.1 `forge` is 114 MB on arm64 and 129 MB on amd64 and works on both. CI runs amd64 only and would not notice a broken arm64 image, so the check requires an artifact for every source file, not only exit code 0. |
| solc 0.8.39 | The latest release, and the only recent one with no known bug in the Solidity bug list; 0.8.38 has two storage copy bugs. Every file uses this exact version. |
| `evm_version = "cancun"` | The newest opcodes the contracts need: transient storage, for the reentrancy guard. |
| No compiler warning hidden | `ignored_error_codes = []`: forge hides the warnings on code size and transient storage by default, and the libraries' warnings are not filtered either, so any warning fails the check. |
| The reentrancy guard in the vault, not OpenZeppelin's | `ReentrancyGuardTransient` writes transient storage in inline assembly, which the compiler warns about; `ReentrancyGuard` keeps a storage slot and is deprecated in OpenZeppelin 5.7. A `transient` state variable is the same mechanism checked by the compiler, in eight lines, with tests that a callback into `deposit` or `withdraw` gets `ReentrantCall`. |
| Unit tests in several contracts | One contract per topic keeps each under the 48 KB initcode limit, which the compiler warns about; the upgrade test deploys the next version from its artifact for the same reason. |
| No metadata hash in the bytecode | The same source always gives the same bytes, so a deployed contract can be compared with a local build. |
| Halmos in its own image, for linux/amd64 | Halmos 0.3.3 pins z3-solver 4.12.6.0, which has no arm64 wheel. The image runs natively in CI and under emulation on an arm64 Mac; the image tag is a hash of the Dockerfile and the requirements, so a change rebuilds it. |
| z3, not Halmos' default yices | Under emulation yices starts but is killed by an illegal instruction as soon as it solves; z3 gives the same answer on both machines. A trial run showed z3 proving a property and finding the counterexample of a false one. |
| State placed by `layout at erc7201(...)` | The compiler puts plain state variables in the ERC-7201 namespace, so there is no assembly storage accessor, and the layout is the compiler's own (`forge inspect`). The compiler lets no contract inherit from one that sets a layout, so the code is `VaultCore` and every version, `Vault` and the test's `VaultV2`, is `VaultCore` in the same namespace. |
| Slither findings reviewed one by one | Slither reports 86 findings, all but one inside OpenZeppelin. Each is recorded in [slither.db.json](slither.db.json), Slither's own triage file, keyed by a hash of the finding and its source lines, and explained in [docs/review/vault.md](../docs/review/vault.md). The check fails on a finding not recorded and on a recorded one that is gone; filtering the libraries out by path would also drop a finding in our code that merely names a library function. |
| Slither in its own image, native | Every package Slither needs has a wheel for amd64 and arm64, so the image is built for the machine it runs on; forge in it is the pinned one. |
| Libraries as git submodules at a tag's commit | Pinned by content, reviewed in the repository like any other change. Installing from a branch is the mistake OpenZeppelin warns about. |

## Files

| Path | Content |
|---|---|
| [foundry.toml](foundry.toml) | Compiler, EVM target, library paths |
| [src/interfaces/IVault.sol](src/interfaces/IVault.sol) | The vault's interface: types, events, errors, functions |
| [src/Vault.sol](src/Vault.sol) | `VaultCore`, the vault's code, built on OpenZeppelin: default admin rules, pause, UUPS, SafeERC20, its own reentrancy guard on a `transient` variable; and `Vault`, that code with its state in its ERC-7201 namespace |
| [test/unit](test/unit) | A test for each rule of the specification, the bad case included, one contract per topic: deposits, withdrawals, reentrancy, queue and pause, administration, upgrades |
| [test/unit/VaultLayout.t.sol](test/unit/VaultLayout.t.sol) | I10: where each field of the state lives, read slot by slot from the proxy |
| [test/fuzz/VaultFuzz.t.sol](test/fuzz/VaultFuzz.t.sol) | The rules over random amounts, ids and times; the rate limit checked over every span |
| [test/invariant](test/invariant) | I1 to I9 over random sequences of every call by every key; the handler keeps its own books |
| [test/symbolic/VaultSymbolic.t.sol](test/symbolic/VaultSymbolic.t.sol) | I1, I2, I3, I4, I7 proved by Halmos for every value of the arguments |
| [test/deploy/VaultDeploy.t.sol](test/deploy/VaultDeploy.t.sol) | The devnet deployment step for step: the timelock as only admin, the first configuration through its delay, a cancelled proposal |
| [test/utils](test/utils) | `VaultSetup`: the vault behind its proxy, configured, as every test starts; `VaultUnitBase`: a funded user and the helpers of the unit tests |
| [test/mocks](test/mocks) | Tokens that misbehave (a fee on transfer, no return value, a callback into the vault), a recipient that refuses ETH, and a next version of the vault for the upgrade test |
| `.gas-snapshot` | Gas of each unit test |
| [slither.db.json](slither.db.json) | The Slither findings reviewed, one entry each; `make contracts-slither-record` rewrites it after a review |
| `lib/` | The libraries above, as submodules |
| `out/`, `cache/` | Build output; not in git |
