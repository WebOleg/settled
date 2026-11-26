# Design: local devnet

How the local network is built and why. This file covers what exists in the repository:
the L1, and the operator keys, contract deployment, sequencer, batcher and verifier of
the L2. It grows as the L2 is added.

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
`genesis.json`, `rollup.json` and `l1-addresses.json` into `.devnet/l2/deployer`, with
the hash of the L1 genesis block in `l1-genesis-hash`.

| Part | Choice | Reason |
|---|---|---|
| Tool | op-deployer v0.9.0, image pinned by digest `sha256:887ddaae...a0e7c8` | The latest stable release with a published image. It deploys op-contracts v9.0.0. |
| Intent type | `custom` | `standard-overrides` refuses an L1 it does not know (`unsupported chainID: 3151908`) |
| L2 chain id | 420420 | Not used by a public network |
| Roles | One operator address per role, see the table above | The intent is rendered by [internal/l2intent](../../internal/l2intent), which refuses a missing role, a zero address, or a role given to the deployer |
| Deployer key | Passed to the container as `-e DEPLOYER_PRIVATE_KEY` with no value | The key never appears on a command line or in the process list |
| Reruns | A finished deployment in `state.json` is left alone; a partial one is deleted and redone | A rerun never deploys a second set of contracts |
| Bound to its L1 | The hash of the L1 genesis block is written to `l1-genesis-hash`. `l2-deploy.sh` and `l2-init.sh` refuse to go on when the running L1 has another genesis | After an L1 reset the old contracts no longer exist, and an L2 started from the old chain config would follow an L1 that is not there. The run stops with `run make reset` instead. |

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

Dispute and withdrawal timings, set in the intent's `globalDeployOverrides`:

| Parameter | Devnet | Production | What it delays |
|---|---|---|---|
| `proofMaturityDelaySeconds` | 60 s | 7 days | Finalizing a withdrawal after it was proven |
| `disputeGameFinalityDelaySeconds` | 30 s | 3.5 days | Using a resolved game for withdrawals; the guardian's window to step in |
| `faultGameMaxClockDuration` | 120 s | 3.5 days | Resolving a game; an unchallenged claim wins when its clock runs out |
| `faultGameClockExtension` | 30 s | 3 hours | Extra time for a move made close to the end of a clock |
| `preimageOracleChallengePeriod` | 30 s | 1 day | Challenging a large preimage proposal |
| `faultGameWithdrawalDelay` | 60 s | 7 days | Withdrawing a bond from DelayedWETH |

All six differ from production, so a withdrawal completes in about 15 minutes instead of
more than a week; most of that is waiting for the L1 to finalize the batches. The
contracts are the same; only the numbers change. Two relations are checked by the
contracts and by a unit test in [internal/l2intent](../../internal/l2intent): twice the
clock extension, and the clock extension plus the preimage challenge period, must each
fit in the max clock duration. `dangerouslyAllowCustomDisputeParameters` lets the chain
deploy with timings that are not the standard ones. op-deployer drops override keys it
does not know without an error, which is why `make l2-verify` reads every timing back
from the L1.

The respected dispute game type is 5, `SUPER_PERMISSIONED_CANNON`, which op-contracts v9
deploys for a new chain. It has no clock and no challenge step (see the proposer
section), so of the timings above it uses only the proof maturity delay and the
finality delay. The max clock, clock extension, preimage challenge period and DelayedWETH
delay apply to the fault dispute games the deployment also holds, which a
permissionless setup would use. A permissionless game needs an absolute prestate built
for this chain; that is a roadmap stage of its own.

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
| AnchorStateRegistry | `respectedGameType()` | 5, `SUPER_PERMISSIONED_CANNON` |
| OptimismPortal | `proofMaturityDelaySeconds()` | 60 |
| AnchorStateRegistry | `disputeGameFinalityDelaySeconds()` | 30 |
| PreimageOracle | `challengePeriod()` | 30 |
| DelayedWETH (permissioned games) | `delay()` | 60 |
| OptimismPortal | `finalizeWithdrawalTransaction` of a withdrawal nobody proved, as `eth_estimateGas` | refused with `OptimismPortal_Unproven`, read by name from the revert data |

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
| Peer-to-peer on, sequencer key as a Compose secret | The sequencer key signs every new block before op-node gossips it, and the verifier accepts only blocks signed by the `unsafeBlockSigner` in SystemConfig. The key is read from a secret file into the environment of op-node when the container starts, as for the batcher. |
| Ports 18545, 18546, 19545 on 127.0.0.1 | Separate from the L1 ports; nothing listens on other interfaces |

The safe and finalized L2 heads come from batches on the L1, which the batcher posts.

## L2: batcher

op-batcher v1.19.0, pinned by digest, reads new L2 blocks from op-reth and op-node,
packs them into channels, and posts each channel to the batch inbox address on the L1.
op-node reads the inbox back, and a block it derives from the L1 becomes safe; it
becomes finalized when the L1 block holding its batch is finalized.

| Choice | Reason |
|---|---|
| Blobs as data availability (`--data-availability-type=blobs`) | Production OP Stack chains post batches as EIP-4844 blobs, which cost less than calldata and are pruned by the L1 after about 18 days. The default is calldata. Since Fusaka a blob sidecar carries cell proofs; op-batcher v1.19.0 builds only that format, which fits the local L1, where Osaka is active from genesis. |
| Span batches (`--batch-type=1`) and `brotli-10` compression | A span batch encodes a range of blocks in one batch (Delta), and brotli compresses better than zlib (Fjord). Both forks are active on this L2. The defaults are the older singular batches and zlib. |
| `--max-channel-duration=5`, 30 s | Differs from production. A channel otherwise closes only when it is full or when the sequencing window is close to its end, so on an idle devnet the safe head would wait for hours. Production chains keep a channel open for hours to fill each blob and pay less. |
| `--num-confirmations=4` | Differs from production, where the default of 10 is used. This L1 has one beacon node and no peers, so it does not reorg unless a reorg is made on purpose. With one transaction in flight, 4 confirmations (24 s) keep up with one channel every 30 s. |
| `--wait-node-sync` | After a restart the batcher waits until op-node has read the L1 up to its head before it posts, so data already on the L1 is not posted twice. |
| Data throttling on, `miner` API on op-reth | When posting falls behind, the batcher asks the execution client through `miner_setMaxDASize` to build blocks with less data, so the backlog cannot grow without limit. op-batcher stops if the method is missing, so op-reth serves the `miner` namespace. Here it is reachable from the Docker network and 127.0.0.1 only; a production node that serves a public RPC keeps it on an internal endpoint. The simpler option, turning throttling off, is what upstream warns against for production. |
| The key as a Compose secret, read into the environment when the container starts | Only the batcher container gets a key. It is not written in `compose.yaml`, an env file or a command line, so it does not show in `docker inspect`, in `docker compose config` or in the process list. This was checked against `docker inspect` and `docker compose config` when the service was added. The simpler options, `--private-key` on the command line or an env file, show the key in `docker inspect`. |
| No published port, metrics off | Nothing on the host calls the batcher yet. Metrics come with the observability stage. |

## L2: verifier

A second op-reth and op-node, with no key of any role. The verifier builds the chain
the way any node that is not the sequencer does: op-node reads the batches from the L1
and derives the blocks, and new blocks that are not batched yet arrive over
peer-to-peer from the sequencer. A block becomes safe on the verifier only when the
verifier has derived it from the L1 itself, so a matching safe head shows that the L1
alone holds the chain. `make l2-check` compares the block hash and the state root of
the verifier's safe and unsafe heads with the sequencer's blocks at the same numbers.

| Choice | Reason |
|---|---|
| `--syncmode=consensus-layer` | Every block goes through derivation in op-node. The other mode lets the execution client fetch state from peers, which would skip the derivation this node exists to check. |
| Fixed peer-to-peer node keys, created once by `l2-init.sh` | A peer ID is derived from the node key, so fixed keys give fixed IDs. `cmd/l2-peer-id` computes the sequencer's ID before either node starts, and the verifier gets the sequencer as a static peer. `make l2-check` asks each op-node which ID it announces and compares. |
| Discovery off, static peer only, port not published | Two nodes in one Docker network need no discovery, and nothing outside it should be able to connect. |
| Own engine API secret | One secret per op-node and op-reth pair, as for the sequencer. |
| `--rollup.sequencer` on the verifier's op-reth | Transactions sent to the verifier are forwarded to the sequencer, the only node that builds blocks. This is how public RPC nodes of OP Stack chains work. |
| Verifier ports 28545, 28546, 29545 on 127.0.0.1 | Separate from the sequencer's ports. |
| `--rpc.eth-proof-window=302400` on the verifier's op-reth | `eth_getProof` for blocks up to one week back. op-reth refuses proofs for older blocks by default (`distance to target block exceeds maximum proof window`), and checking a proposal or proving a withdrawal needs the storage proof at the block the proposal names. |
| Verification confirmations left at 0 | The verifier derives from the L1 head, as the sequencer does. Nodes that follow a public L1 often wait a few L1 blocks to avoid work undone by L1 reorgs; this L1 does not reorg on its own. |

## L2: proposer

op-proposer v1.17.1, pinned by digest, posts the state of the L2 to the L1 as dispute
games. Each game claims a super root at an L2 timestamp: version 1, the timestamp, and
for each chain its chain id and output root. With one chain it wraps one output root,
which commits to the block's state root, its hash and the storage root of the
L2ToL1MessagePasser, where withdrawals are recorded. A withdrawal is proven on the L1
against such a claim once its game has resolved.

| Choice | Reason |
|---|---|
| Game type 5, `SUPER_PERMISSIONED_CANNON` | The type the deployment respects. Only the proposer can create a game, and the game resolves for the proposer the moment it is created: it has no clock, no bond and no challenge step. A wrong root is removed by the guardian, who blacklists the game in the AnchorStateRegistry during the finality delay, before any withdrawal can use it. Safety rests on the proposer and on someone checking every game in time. |
| `--superroot-rpcs` pointing at the verifier's op-node | Game type 5 needs super roots, and op-node serves `superroot_atTimestamp` for a single chain. The proposer reads from the node that derives the chain from the L1, not from the sequencer that built it, so it never proposes what only the sequencer has seen. |
| Finalized L2 only (`--allow-non-finalized` off) | A proposal covers only blocks whose batches are in finalized L1 blocks, so an L1 reorg cannot invalidate it. The first game appears once the L1 has finalized, about 10 minutes after a new devnet starts. |
| `--proposal-interval=60s` | Differs from production, where a proposal is made about once an hour; each game costs L1 gas. It is a minimum: a game is made only when the finalized L2 head has moved, and L1 finality moves in steps of an epoch or two, so games come about every 6 minutes here, about 200 L2 blocks apart. |
| `--num-confirmations=4` | As for the batcher: this L1 does not reorg on its own. |
| The key as a Compose secret | As for the batcher and the sequencer: only this container gets it, and it is not in `docker inspect` or the process list. |
| The factory address in an env file | It changes with every deployment; `l2-init.sh` writes it from `l1-addresses.json`. It is not a secret. |

That someone is `make l2-games`: it checks the most recent games without trusting the
proposer or op-node. For each game it reads from the L1 the type, the creator and the
proposer the game accepts, that it resolved for the defender and that its type was the
respected one, and the root claim. It then takes the
block at the game's timestamp from the verifier's op-reth, gets the
L2ToL1MessagePasser storage root with `eth_getProof`, and recomputes the output root
and the super root with [internal/outputroot](../../internal/outputroot). The output
root must also equal what the verifier's op-node reports, and the root claim must equal
the recomputed super root. The encoding was checked against a real block of this L2:
the output root computed from op-reth data matched the one op-node returned.

## L2: deposits

`make l2-deposit` moves 1 ETH from the devnet user on the L1 to the same address on the
L2, with [cmd/l2-deposit](../../cmd/l2-deposit). The user is index 2 of the public
devnet mnemonic; index 0 funds the operators, so the user has its own nonces.

The path of a deposit: `OptimismPortal.depositTransaction` takes the ETH, keeps it in
custody on the L1 (in the ETHLockbox) and emits an event. op-node reads the event while
it derives the L2 from the L1 and puts a deposit transaction (type `0x7e`) at the start
of an L2 block; that transaction mints the ETH on the L2. No key on the L2 is involved,
and the sequencer cannot leave a deposit out: every node derives it from the same L1
block.

| Check | Expected, exactly |
|---|---|
| ETHLockbox and OptimismPortal together, at the deposit block against the block before | the amount more |
| The user on the L1, same two blocks | the amount plus gas used times the effective gas price less |
| The user on the L2 | the amount more, in one block |
| That L2 block | holds a deposit transaction to the user that mints exactly the amount |
| The verifier's safe head | reaches that block: the credit is derived from the L1 |

| Choice | Reason |
|---|---|
| Amounts in wei, balances read at fixed blocks | A balance read at "latest" can include anything else that happened meanwhile. Reading the block before and the block of the deposit makes the difference exactly what the deposit did. |
| The deposit counts once it is safe | An unsafe block is only the sequencer's word. A safe block is derived from the L1 by the verifier. The bridge in stage 5 goes one step further and credits only at finalized. |
| `OptimismPortal` directly, not `L1StandardBridge` | The portal is the base layer every bridge uses. The standard bridge adds a cross-domain message on top, which is worth showing with tokens, not with plain ETH. |

## L2: withdrawals

`make l2-withdraw` takes 0.1 ETH of the devnet user back from the L2 to the L1, with
[cmd/l2-withdraw](../../cmd/l2-withdraw). It takes about 15 minutes, most of it waiting
for the L1 to finalize the block that holds the withdrawal, because the proposer only
proposes finalized state.

| Step | Where | What is checked, exactly |
|---|---|---|
| Start | L2: `L2ToL1MessagePasser.initiateWithdrawal`, sent to the verifier, which forwards it to the sequencer | The withdrawal hash computed here equals the one the contract emits. The user paid the amount, the execution gas and the L1 data fee (`l1Fee` of the receipt). The passer holds the amount more. |
| Wait for a game | L1: the newest game at or after the withdrawal's L2 block | Type 5, created by the proposer, resolved for the defender, respected; its root claim equals the super root recomputed from the verifier. A game is never used unchecked. |
| Prove | L1: `OptimismPortal.proveWithdrawalTransaction` | The proof is the output root opened into state root, passer storage root and block hash, and the Merkle proof from `eth_getProof` that the withdrawal's slot is set at the game's block. |
| Wait for maturity | L1 | Finalization is tried with `eth_estimateGas` until the portal accepts it: 60 s after the proof, and 30 s after the game resolved. |
| Finalize | L1: `OptimismPortal.finalizeWithdrawalTransaction` | The lockbox and the portal paid out the amount; the user received it less the gas; `finalizedWithdrawals(hash)` is true. |
| Replay | L1 | A second finalization of the same withdrawal is refused. |
| Overall | L1 | From the block before the proof to the block of the finalization, the user gained the amount less the gas of both transactions. |

The withdrawal hash commits to nonce, sender, target, value, gas limit and data, and the
portal marks it finalized before it pays. That is what makes a withdrawal pay once,
however many times its finalization is sent. The bridge in stage 5 adds the same rule
on its own side: one payout per withdrawal in the ledger, under retries and races.

## Alternatives rejected

| Option | Why not |
|---|---|
| A development L1 such as anvil or `geth --dev` | Blocks are not built by a consensus client, so `safe` and `finalized` do not come from attestations. Finality handling would be tested against a fake. |
| Kurtosis (`ethereum-package`, `optimism-package`) | Starts a full network in one command, but hides every flag and file behind a package. This project needs to show and own each part of the setup. |
| Prysm as consensus client | Works, but needs separate images for the beacon node and the validator client. |

## Known limits

- One L1 node, so the L1 has no peer-to-peer traffic and no natural reorg. A reorg has
  to be produced on purpose. The L2 has two nodes, and their peer-to-peer traffic
  stays inside the Docker network.
- With no peers, the beacon node logs `Low peer count` and `NoPeersSubscribedToTopic`
  warnings on every slot. They are expected here.
- `make l1-check` judges sync by how far the head is behind the current slot
  (`sync_distance`), the direct measure. The `is_syncing` flag is only printed: in the
  first minutes after genesis Lighthouse reported it as true with a sync distance of 0,
  and later as false.
- The validator keys are public test keys. They are safe only on this network.
- The suggested fee recipient is the burn address `0x...dEaD`. Priority fees on this
  network have no value.
- The batcher and sequencer keys sit in files on the same machine. Anyone who can run
  `docker exec` or read `.devnet/l2/keys` can read them. Production batchers sign
  through a remote signer (`--signer.*`), so the key never reaches the host that runs
  the batcher.
- With game type 5 a wrong root is only stopped if someone recomputes it and the
  guardian blacklists the game within the 30 s finality delay. `make l2-games` does the
  recomputation when it is run; nothing yet does it continuously or acts on it. The
  first check in this repository assumed a game with a clock and failed reading
  `maxClockDuration()`: the expectation was wrong, not the game.
- If the L2 runs without a batcher for longer than the sequencing window (3600 L1
  blocks, 6 hours here), op-node replaces the unbatched blocks when the batcher comes
  back. `make l2-reset` starts the L2 over in that case.
