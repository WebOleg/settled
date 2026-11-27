# Contracts

The Solidity contracts of the exchange on the L2. What they do and the invariants they
keep are in [docs/design/vault.md](../docs/design/vault.md).

## Commands

| Command | What it does |
|---|---|
| `make check-contracts` | Checks formatting, builds without compiler warnings, checks that every source file built an artifact, runs the tests, and compares the gas of the unit tests with `.gas-snapshot`; part of `make check` |
| `make contracts-snapshot` | Records the gas of the unit tests in `.gas-snapshot`. A change in gas shows in review as a change to that file. |

Forge runs in the pinned Foundry image, as the calling user, with its home folder on
`.cache/foundry` (where solc is kept after the first download). Nothing is installed on
the host.

## Toolchain

| Tool | Version | Pinned by |
|---|---|---|
| Foundry | v1.9.0 | image digest in the [Makefile](../Makefile) |
| solc | 0.8.39 | [foundry.toml](foundry.toml); downloaded and checked by forge |
| OpenZeppelin Contracts | v5.7.0 | git submodule at the tag's commit |
| OpenZeppelin Contracts Upgradeable | v5.7.0 | git submodule at the tag's commit |
| forge-std | v1.17.0 | git submodule at the tag's commit |

| Choice | Reason |
|---|---|
| Foundry v1.9.0, not v1.10.0 | In the arm64 image of v1.10.0, `forge`, `cast` and `chisel` are 434 KB files that print nothing and exit 0; in v1.9.0 `forge` is 110 MB and works. The amd64 image of v1.10.0 is fine, so CI alone would not have noticed. The check therefore requires an artifact for every source file, not only exit code 0. |
| solc 0.8.39 | The latest release, and the only recent one with no known bug in the Solidity bug list; 0.8.38 has two storage copy bugs. Every file uses this exact version. |
| `evm_version = "cancun"` | The newest opcodes the contracts need: transient storage, for the reentrancy guard. |
| No metadata hash in the bytecode | The same source always gives the same bytes, so a deployed contract can be compared with a local build. |
| Libraries as git submodules at a tag's commit | Pinned by content, reviewed in the repository like any other change. Installing from a branch is the mistake OpenZeppelin warns about. |

## Files

| Path | Content |
|---|---|
| [foundry.toml](foundry.toml) | Compiler, EVM target, library paths |
| [src/interfaces/IVault.sol](src/interfaces/IVault.sol) | The vault's interface: types, events, errors, functions |
| [src/Vault.sol](src/Vault.sol) | The vault, built on OpenZeppelin: default admin rules, pause, UUPS, transient reentrancy guard, SafeERC20 |
| [test/unit/Vault.t.sol](test/unit/Vault.t.sol) | A test for each rule of the specification, the bad case included |
| [test/mocks](test/mocks) | Tokens that misbehave (a fee on transfer, no return value, a callback into the vault), a recipient that refuses ETH, and a next version of the vault for the upgrade test |
| `.gas-snapshot` | Gas of each unit test |
| `lib/` | The libraries above, as submodules |
| `out/`, `cache/` | Build output; not in git |
