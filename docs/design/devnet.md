# Design: local devnet

How the local network is built and why. This file covers what exists in the repository:
the L1. It grows as the L2 is added.

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
  (`sync_distance`), the direct measure. The `is_syncing` flag is only printed: Lighthouse
  may report it as true on a node that has no peers to sync from.
- The validator keys are public test keys. They are safe only on this network.
- The suggested fee recipient is the burn address `0x...dEaD`. Priority fees on this
  network have no value.
