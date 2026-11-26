# Design: local devnet

How the local network is built and why. This file covers what exists in the repository:
the L1, and the operator keys, contract deployment and sequencer of the L2. It grows as
the L2 is added.

## Goal

A local network that behaves like the real one in the ways this project depends on:
blocks are built by a consensus client, `safe` and `finalized` come from real attestations,
and blob transactions are accepted. A network that fakes any of these would make the later
work (finality handling, batch submission, reorg drills) untestable.

## L1: decision

A proof-of-stake network started from genesis, with the clients and settings below.

| Part | Choice | Reason |
|---|---|---|
| Execution client | geth v1.17.10 | The reference execution client; arm64 image available |
| Consensus client | Lighthouse v8.3.3 | Beacon node and validator client ship in one image |
| Genesis | ethereum-genesis-generator 6.2.9 | Builds matching execution and consensus genesis and validator keys in one tool |
| Forks | Fulu and Osaka (Fusaka) active at genesis; Gloas and Amsterdam (Glamsterdam) off | Matches mainnet when this was written: Fusaka active, Glamsterdam without a mainnet date. The L2 must run against the L1 rules it will meet in production. |
| Blob schedule | Generator defaults: blob-parameter forks `bpo1` and `bpo2` active at genesis | Not verified against the current mainnet blob limits |
| Genesis time | The moment `l1-init.sh` runs, plus 30 s | The generator default dates genesis to 1970; Lighthouse then treats the chain as too old to trust and refuses to start |
| Chain id | 3151908 | Common local L1 id; does not clash with any public network |

## L1: settings that differ from mainnet

| Setting | Value | Mainnet | Why |
|---|---|---|---|
| Slot duration | 6 s | 12 s | Finality arrives in about half the time, which keeps finality tests short. Blocks, attestations and finality work the same way. |
| Validators | 64 | about one million | Enough for a valid chain; keeps memory and start-up time low |
| Peers | none | many | One node per layer; there is no one to connect to |
| Beacon node mode | `--supernode` | optional | With PeerDAS a node must hold the blob data columns. A single node has to hold all of them, or blob data would be unavailable. |
| Sync | `--allow-insecure-genesis-sync` | checkpoint sync | The network starts at its own genesis, so there is no checkpoint to sync from |

## L1: blob transactions

`make l1-blob-check` sends one blob transaction and waits for its inclusion. It is a Go
command, [cmd/l1-blob-check](../../cmd/l1-blob-check), not a third-party CLI:

- It builds a version 1 sidecar: the blob, its KZG commitment and 128 cell proofs
  (EIP-7594), the format introduced by Fusaka.
- It derives the sender key from the public devnet mnemonic with BIP39 and BIP32,
  tested against widely published addresses.
- It passes only when the receipt shows blob gas used and the block accounts for it.

The Foundry image `ghcr.io/foundry-rs/foundry:v1.10.0` was considered first. Its `cast`,
`forge` and `chisel` binaries are about 430 KB each and print nothing, while `anvil` is
53 MB, so the image was not usable. The `stable` tag pointed to v1.5.1 from December
2025.

## L2: operator roles and keys

The L2 has one key per role, so that no key holds more power than its job needs. The
split follows the role fields of the op-deployer intent file.

| Role | Intent field | Can | Balance on L1 |
|---|---|---|---|
| `deployer` | none, it signs the deployment | deploy the contracts; holds no role after that | 100 ETH |
| `superchain-admin` | `SuperchainProxyAdminOwner` | upgrade the superchain contracts | 0 |
| `guardian` | `SuperchainGuardian` | pause the system in an emergency | 0 |
| `chain-admin` | `l1ProxyAdminOwner`, `l2ProxyAdminOwner` | upgrade this chain's contracts | 0 |
| `system-config-owner` | `systemConfigOwner` | change chain parameters such as the gas limit | 0 |
| `sequencer` | `unsafeBlockSigner` | sign unsafe blocks for peer-to-peer gossip | 0 |
| `batcher` | `batcher` | post batches to the L1 | 20 ETH |
| `proposer` | `proposer` | post output roots to the L1 | 20 ETH |
| `challenger` | `challenger` | dispute invalid output roots | 20 ETH |
| `fee-recipient` | the four fee vault recipients | receive L2 fees | 0 |

How the keys are handled, in [internal/rolekeys](../../internal/rolekeys):

- `make l2-keys` creates a missing key from `crypto/rand` and keeps every existing one.
  Files are mode 600 in `.devnet/l2/keys` (mode 700) and are opened with `O_EXCL`, so a
  key is never replaced, not even by a second run racing the first.
- A key that others can read is refused. A wrong permission shows up as an error before
  anything uses the key.
- Keys are never printed. `addresses.json` in the same folder lists the addresses.

`make l2-fund` tops the four paying accounts up to the balances above from the first
devnet account, in [internal/funding](../../internal/funding). It sends only the
difference, so running it again sends nothing to an account that is already funded.

## L2: contract deployment

`make l2-deploy` deploys the L2 contracts to the local L1 with op-deployer and exports
`genesis.json`, `rollup.json` and `l1-addresses.json` into `.devnet/l2/deployer`.

| Part | Choice | Reason |
|---|---|---|
| Tool | op-deployer v0.9.0, image pinned by digest `sha256:887ddaae...a0e7c8` | The latest stable release with a published image. It deploys op-contracts v9.0.0. |
| Intent type | `custom` | `standard-overrides` refuses an L1 it does not know (`unsupported chainID: 3151908`) |
| L2 chain id | 420420 | Not used by a public network |
| Roles | One operator address per role, see the table above | The intent is rendered by [internal/l2intent](../../internal/l2intent), which refuses a missing role, a zero address, or a role given to the deployer |
| Deployer key | Passed to the container as `-e DEPLOYER_PRIVATE_KEY` with no value | The key never appears on a command line or in the process list |
| Reruns | A finished deployment in `state.json` is left alone; a partial one is deleted and redone | A rerun never deploys a second set of contracts |

Chain parameters in the intent:

| Parameter | Value | Source |
|---|---|---|
| `eip1559Denominator` | 50 | op-deployer standard configuration |
| `eip1559DenominatorCanyon` | 250 | op-deployer standard configuration |
| `eip1559Elasticity` | 6 | op-deployer standard configuration |
| `gasLimit` | 60,000,000 | op-deployer default; the standard configuration allows up to 200,000,000 |
| `operatorFeeScalar`, `operatorFeeConstant` | 0 | A non-zero operator fee makes a chain non-standard |
| `minBaseFee` | 0 | Jovian default |
| `daFootprintGasScalar` | 400 | Jovian treats 0 as 400; written out so the file states the value in use |

Why not another op-deployer version, as checked on 2026-11-26:

| Option | Why not |
|---|---|
| v0.10.0 (op-contracts v10.0.0, Upgrade 22) | Released the day before with no binaries and no image in the registry. Building it from source needs files the build generates (`superchain-configs.zip`, forge artifacts), which means the full Optimism contracts build. |
| The `latest` image tag | It is op-deployer v0.6.0 from April 2026, not the latest release |
| Pre-release tags (`-dev-test`, `-allowlist`, `-op-private-test`) | Test builds, not releases |

Moving to op-contracts v10.0.0 is an upgrade of a running chain, which is a stage of the
roadmap of its own.

`--no-verify` is passed to `apply`: there is no block explorer on the devnet to publish
contract sources to, and without the flag op-deployer ends a successful deployment with
error lines about verification.

## L2: deployment check

The intent says who should own what; `make l2-verify` checks what was actually deployed,
with [cmd/l2-verify](../../cmd/l2-verify). It needs no key.

On the L1, read from the contracts:

| Contract | Getter | Expected |
|---|---|---|
| SuperchainProxyAdmin | `owner()` | `superchain-admin` |
| SuperchainConfig | `guardian()` | `guardian` |
| Chain ProxyAdmin | `owner()` | `chain-admin` |
| DisputeGameFactory | `owner()` | `chain-admin` |
| SystemConfig | `owner()` | `system-config-owner` |
| SystemConfig | `batcherHash()` | `batcher`, as a bytes32 word |
| SystemConfig | `unsafeBlockSigner()` | `sequencer` |
| SystemConfig | `gasLimit()` | 60,000,000 |
| SystemConfig | `eip1559Denominator()`, `eip1559Elasticity()` | 0, unset |
| SystemConfig | `daFootprintGasScalar()` | 0, unset |
| SystemConfig | `minBaseFee()`, `operatorFeeScalar()` | 0 |

It also fails if the deployer address holds any of these roles.

The zeros are by design. The Holocene SystemConfig does not set the EIP-1559 parameters
at initialisation, and while they are unset the L2 uses the values of its chain config.
For the DA footprint scalar, Jovian treats 0 as 400. The first run of `l2-verify`
expected the intent values on SystemConfig and failed on these three; the expectation
was wrong, not the deployment. op-deployer v0.9.0 does not write the intent's
`daFootprintGasScalar` to SystemConfig either.

In the exported chain config, where those values live, and where the L2 nodes will look
for the contracts:

| File | Field | Expected |
|---|---|---|
| `genesis.json` | `config.chainId` | 420420 |
| `genesis.json` | `config.optimism.eip1559Denominator`, `eip1559DenominatorCanyon`, `eip1559Elasticity` | 50, 250, 6 |
| `rollup.json` | `l2_chain_id`, `l1_chain_id` | 420420, 3151908 |
| `rollup.json` | `chain_op_config.eip1559Denominator`, `eip1559DenominatorCanyon`, `eip1559Elasticity` | 50, 250, 6 |
| `rollup.json` | `l1_system_config_address` | the deployed SystemConfigProxy |
| `rollup.json` | `deposit_contract_address` | the deployed OptimismPortalProxy |
| `rollup.json` | `genesis.system_config.batcherAddr` | `batcher` |

## L2: sequencer

`make l2-up` starts op-reth v2.10.0 and op-node v1.21.0, both pinned by digest. Details
of the services, endpoints and files are in
[infra/devnet/l2/README.md](../../infra/devnet/l2/README.md).

| Choice | Reason |
|---|---|
| op-reth as execution client | op-geth reached end of support on 31 May 2026 and does not follow the chain after the Karst hardfork; op-reth is the supported client |
| `--rollup.l1-chain-config` gets the `config` object of the L1 genesis, limited to the fields op-node knows | op-node knows the forks and blob parameters of public L1s only; the local L1 has to be described to it. op-node refuses the whole genesis file (`unknown field "config"`), so `l2-init.sh` extracts the object. op-node also decodes it strictly, and the generator writes `terminalTotalDifficultyPassed`, which go-ethereum has removed; `l2-init.sh` keeps only the fields of go-ethereum's `ChainConfig` at commit `3dee18e7108c`, the version op-node v1.21.0 is built on, and prints what it drops. The list is rechecked when op-node is upgraded. |
| `--safedb.path` set | Keeps which L2 block became safe at which L1 block, which the proposer and challenger ask for |
| One engine API secret per op-node and op-reth pair, mode 600 | A secret shared across pairs would let one node's credentials drive another engine |
| Peer-to-peer off, no sequencer key in the container | The sequencer key signs unsafe blocks for gossip to other nodes. With no other node there is nothing to gossip to, and a container gets no key it does not use. |
| Ports 18545, 18546, 19545 on 127.0.0.1 | Separate from the L1 ports; nothing listens on other interfaces |

Until a batcher posts batches to the L1, the safe and finalized L2 heads stay at genesis.
`make l2-check` prints them and does not treat that as a failure.

## Alternatives rejected

| Option | Why not |
|---|---|
| A development L1 such as anvil or `geth --dev` | Blocks are not built by a consensus client, so `safe` and `finalized` do not come from attestations. Finality handling would be tested against a fake. |
| Kurtosis (`ethereum-package`, `optimism-package`) | Starts a full network in one command, but hides every flag and file behind a package. This project needs to show and own each part of the setup. |
| Prysm as consensus client | Works, but needs separate images for the beacon node and the validator client. |

## Known limits

- One node per layer, so there is no peer-to-peer traffic and no natural reorg. A reorg
  has to be produced on purpose.
- With no peers, the beacon node logs `Low peer count` and `NoPeersSubscribedToTopic`
  warnings on every slot. They are expected here.
- `make l1-check` judges sync by how far the head is behind the current slot
  (`sync_distance`), the direct measure. The `is_syncing` flag is only printed: in the
  first minutes after genesis Lighthouse reported it as true with a sync distance of 0,
  and later as false.
- The validator keys are public test keys. They are safe only on this network.
- The suggested fee recipient is the burn address `0x...dEaD`. Priority fees on this
  network have no value.
