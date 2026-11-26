# Local L2

The sequencer of the devnet L2: op-reth executes blocks, op-node builds them every 2 s,
follows the L1 and drives op-reth through the engine API. Why it is built this way is
in [docs/design/devnet.md](../../../docs/design/devnet.md#l2-sequencer).

It needs the running L1 (`make l1-up`) and the deployment (`make l2-deploy`).

## Commands

| Command | What it does |
|---|---|
| `make l2-up` | Prepares the runtime folders and the engine API secret on first run, then starts the sequencer |
| `make l2-check` | Shows that the L2 answers, advances every 2 s, follows the L1, and that op-reth and op-node agree on the head |
| `make l2-logs` | Follows the logs of both services |
| `make l2-down` | Stops the sequencer and keeps its data |
| `make l2-reset` | Stops the sequencer and deletes its chain data; keys and deployment are kept |

## Services

| Service | Image | Role |
|---|---|---|
| `op-reth` | `op-reth:v2.10.0`, pinned by digest | Execution client |
| `op-node` | `op-node:v1.21.0`, pinned by digest | Rollup node in sequencer mode |

## Endpoints

| Endpoint | Address |
|---|---|
| L2 execution JSON-RPC | `http://127.0.0.1:18545` |
| L2 execution WebSocket | `ws://127.0.0.1:18546` |
| op-node RPC | `http://127.0.0.1:19545` |

Every port is bound to `127.0.0.1`. The engine API between op-node and op-reth stays
inside the Docker network. op-node also joins the L1 network to reach `geth` and
`beacon`.

## Files

| Path | Content |
|---|---|
| `.devnet/l2/deployer/genesis.json`, `rollup.json` | Chain config from `make l2-deploy`, mounted read-only |
| `.devnet/l2/sequencer/l1-chain-config.json` | The `config` object of the L1 genesis, written by `l2-init.sh` for op-node, mounted read-only |
| `.devnet/l2/sequencer/jwt.hex` | Engine API secret for this pair, mode 600 |
| `.devnet/l2/sequencer/op-reth`, `op-node` | Client databases; op-node keeps its safe-head database here |
