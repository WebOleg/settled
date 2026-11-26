# Local L1

A proof-of-stake Ethereum network on one machine: one execution client, one beacon
node and one validator client with 64 validators. It starts from proof of stake at
genesis, with the same forks active as Ethereum mainnet. Why it is built this way is in
[docs/design/devnet.md](../../../docs/design/devnet.md).

## Commands

| Command | What it does |
|---|---|
| `make l1-up` | Creates the genesis on first run, then starts the network |
| `make l1-check` | Shows that both layers answer, advance and agree, and prints the finality status |
| `make l1-blob-check` | Sends one blob transaction and waits until it is included with blob gas used |
| `make l1-logs` | Follows the logs of all services |
| `make l1-down` | Stops the L2, which uses this network, then the network; keeps the data |

To delete the L1 data use `make reset`, which also deletes the L2 data and the
deployment: both belong to this L1 and are useless after it is replaced.

## Services

| Service | Image | Role |
|---|---|---|
| `geth-init` | `ethereum/client-go:v1.17.10` | Writes the genesis block into the execution database, then exits |
| `geth` | `ethereum/client-go:v1.17.10` | Execution client |
| `beacon` | `sigp/lighthouse:v8.3.3` | Beacon node |
| `validator` | `sigp/lighthouse:v8.3.3` | Validator client for all 64 validators |

Genesis and keystores are created by [scripts/devnet/l1-init.sh](../../../scripts/devnet/l1-init.sh)
with `ethpandaops/ethereum-genesis-generator:6.2.9`, using
[values.env](values.env).

## Endpoints

| Endpoint | Address |
|---|---|
| Execution JSON-RPC | `http://127.0.0.1:8545` |
| Execution WebSocket | `ws://127.0.0.1:8546` |
| Beacon API | `http://127.0.0.1:4000` |

Every port is bound to `127.0.0.1`. The engine API between `geth` and `beacon` stays
inside the Docker network.

## Files

All runtime state lives in `.devnet/l1`, which git ignores:

| Path | Content |
|---|---|
| `.devnet/l1/genesis/metadata` | `genesis.json`, `config.yaml`, `genesis.ssz` and the rest of the generator output |
| `.devnet/l1/genesis/jwt/jwtsecret` | Shared secret for the engine API |
| `.devnet/l1/genesis/keys` | Validator keystores; the validator client also keeps its slashing protection database here |
| `.devnet/l1/geth`, `.devnet/l1/beacon` | Client databases |

The validator keys come from the generator's public test mnemonic. They exist only on
this network and must never hold value anywhere else.
