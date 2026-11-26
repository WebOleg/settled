# Local L2

The devnet L2. The sequencer: op-reth executes blocks, op-node builds them every 2 s,
follows the L1 and drives op-reth through the engine API, and op-batcher posts the
blocks to the L1 as blobs, from which they become safe. The verifier: a second op-reth
and op-node that derive the chain from the L1 and take new blocks from the sequencer
over peer-to-peer. The proposer: op-proposer posts the verifier's view of the chain to
the L1 as dispute games, which withdrawals are proven against. Why it is built this
way is in the [sequencer](../../../docs/design/devnet.md#l2-sequencer),
[batcher](../../../docs/design/devnet.md#l2-batcher),
[verifier](../../../docs/design/devnet.md#l2-verifier) and
[proposer](../../../docs/design/devnet.md#l2-proposer) sections of the design.

It needs the running L1 (`make l1-up`), the role keys (`make l2-keys`, `make l2-fund`)
and the deployment (`make l2-deploy`).

## Commands

| Command | What it does |
|---|---|
| `make l2-up` | Prepares the runtime folders, engine API secrets and node keys on first run, writes the verifier's peer address and the proposer's factory address, checks the role keys, then starts the six services |
| `make l2-check` | Shows that the L2 answers, advances every 2 s and follows the L1; that the safe head advances; that the batches on the L1 are blob transactions to the batch inbox; that op-reth and op-node agree on the unsafe and safe blocks; and that the verifier is connected to the sequencer, catches up, and has the same block hashes and state roots |
| `make l2-games` | Waits up to 15 minutes for the first dispute game, then checks the recent games: type, creator, resolved for the proposer, respected type, and that each root claim equals the super root recomputed from the verifier |
| `make l2-deposit` | Deposits 1 ETH from the devnet user on the L1 to the L2 and checks every wei on both sides, then waits until the credit is safe |
| `make l2-withdraw` | Withdraws 0.1 ETH of the devnet user from the L2 to the L1: start, wait for a game, prove, finalize, refuse a replay; every wei checked; about 15 minutes |
| `make l2-logs` | Follows the logs of all services |
| `make l2-down` | Stops the services and keeps their data |
| `make l2-reset` | Stops the services and deletes the chain data; keys and deployment are kept |

## Services

| Service | Image | Role |
|---|---|---|
| `op-reth` | `op-reth:v2.10.0`, pinned by digest | Execution client |
| `op-node` | `op-node:v1.21.0`, pinned by digest | Rollup node in sequencer mode |
| `op-batcher` | `op-batcher:v1.19.0`, pinned by digest | Posts L2 blocks to the L1 as blob transactions |
| `verifier-op-reth` | `op-reth:v2.10.0`, pinned by digest | Execution client of the verifier; forwards transactions to the sequencer |
| `verifier-op-node` | `op-node:v1.21.0`, pinned by digest | Rollup node in verifier mode |
| `op-proposer` | `op-proposer:v1.17.1`, pinned by digest | Posts the verifier's super roots of finalized state to the L1 as dispute games, at most once a minute |

## Endpoints

| Endpoint | Address |
|---|---|
| L2 execution JSON-RPC | `http://127.0.0.1:18545` |
| L2 execution WebSocket | `ws://127.0.0.1:18546` |
| op-node RPC | `http://127.0.0.1:19545` |
| Verifier execution JSON-RPC | `http://127.0.0.1:28545` |
| Verifier execution WebSocket | `ws://127.0.0.1:28546` |
| Verifier op-node RPC | `http://127.0.0.1:29545` |

Every port is bound to `127.0.0.1`. Metrics (op-reth 9001, op-node, op-batcher and
op-proposer 7300) are served inside the Docker network only, for Prometheus. The engine APIs and the peer-to-peer port 9222 of
both op-nodes stay inside the Docker network. Both op-nodes and op-batcher also join
the L1 network to reach `geth` and `beacon`, and so does op-proposer. op-batcher and
op-proposer publish no port.

## Files

| Path | Content |
|---|---|
| `.devnet/l2/deployer/genesis.json`, `rollup.json` | Chain config from `make l2-deploy`, mounted read-only |
| `.devnet/l2/deployer/l1-genesis-hash` | Genesis hash of the L1 the deployment was made on; `l2-init.sh` refuses any other L1 |
| `.devnet/l2/sequencer/l1-chain-config.json` | The `config` object of the L1 genesis, written by `l2-init.sh`, mounted read-only into both op-nodes |
| `.devnet/l2/sequencer/jwt.hex`, `.devnet/l2/verifier/jwt.hex` | Engine API secret of each pair, mode 600 |
| `.devnet/l2/sequencer/p2p.key`, `.devnet/l2/verifier/p2p.key` | Peer-to-peer node key of each op-node, mode 600, mounted read-only |
| `.devnet/l2/sequencer/peer-id`, `.devnet/l2/verifier/peer-id` | Peer ID derived from each node key by `cmd/l2-peer-id` |
| `.devnet/l2/verifier/p2p.env` | `OP_NODE_P2P_STATIC`, the sequencer's address with its peer ID, for the verifier op-node |
| `.devnet/l2/sequencer/op-reth`, `op-node`, and the same under `verifier` | Client databases; each op-node keeps its safe-head database and peer store here |
| `.devnet/l2/keys/batcher.key` | Batcher key from `make l2-keys`, mode 600; mounted into op-batcher only, as a Compose secret |
| `.devnet/l2/keys/proposer.key` | Proposer key from `make l2-keys`, mode 600; mounted into op-proposer only, as a Compose secret |
| `.devnet/l2/proposer/proposer.env` | `OP_PROPOSER_GAME_FACTORY_ADDRESS` of this deployment, written by `l2-init.sh` |
| `.devnet/l2/keys/sequencer.key` | Sequencer key from `make l2-keys`, mode 600; mounted into the sequencer op-node only, as a Compose secret |
