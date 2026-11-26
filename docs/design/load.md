# Design: load

How the devnet is put under load and what is measured. The tool is
[cmd/l2-load](../../cmd/l2-load/main.go), run with `make l2-load`.

## What it measures

A payment platform needs two numbers from its chain: how many transfers a second it
takes, and how long until a transfer can be treated as settled. On an OP Stack L2 there
are two answers to the second: the block that includes it (unsafe, seconds) and the
block becoming safe, derived from batches on the L1 (about a minute here). A deposit
or a payout is credited at safe or later, never at inclusion.

| Number | How |
|---|---|
| Sent rate | Transactions handed to the sequencer per second, by the clock of the tool. |
| Included rate | Transactions included, from the first send to the last inclusion. |
| Inclusion latency | From the send to the moment the tool first sees the block that holds the transaction. The tool reads every block's receipts, polling every 200 ms. |
| Safe latency | From the send to the moment the sequencer's op-node first reports that block as safe, polled every 500 ms. |
| Per block | The most of these transactions in one block, and the most gas a block used. |

Latencies are reported as p50, p95, p99 and max, by the nearest-rank method
([internal/loadstat](../../internal/loadstat/loadstat.go)), so every number reported was
measured.

## How it runs

| Choice | Reason |
|---|---|
| Plain transfers of 0 wei to the sender itself | The cheapest transaction there is, 21,000 gas. It loads the sequencer, the batcher and derivation, not a contract. Only fees are spent. |
| Sent straight to the sequencer's op-reth | It measures the chain, not a proxy in front of it. |
| Accounts at index 100 and up of the devnet mnemonic | Nothing else uses them, so their nonces are the tool's alone. They are topped up on the L2 from the devnet user before each run; `make l2-deposit` refills the user. |
| Each account sends at most 5 a second, at even offsets | op-reth keeps a limited number of pending transactions per account. With 2 s blocks, 5 a second keeps each account well under it, and the load is spread over each second instead of bursts. |
| A send error stops that account | A missing nonce would hold back every later transaction of the account; the error is reported and the run fails. |
| Receipts read per block with `eth_getBlockReceipts`, decoding only hash, status and gas | One call per block instead of one per transaction, and the OP Stack fields of a receipt do not get in the way. |

A run passes only if every transaction is sent, included and successful, the included
rate is at least 95% of the target, and every block that holds one becomes safe.

## Results

Measured on an Apple M4 Pro (14 cores, 48 GB) with Docker given 10 CPUs and 23 GB; the
tool, both layers and the monitoring share that machine. Each run is two minutes of
plain transfers.

| Run | Batcher | Included | Inclusion p50 / p95 / max | Safe p50 / p95 / max | Alerts |
|---|---|---|---|---|---|
| 50 tx/s, 16 accounts | 1 blob a transaction, 1 in flight | 6,000 of 6,000, 49.1 tx/s | 2.1 / 3.0 / 3.1 s | 50.4 / 61.2 / 62.4 s | none |
| 200 tx/s, 40 accounts | 1 blob a transaction, 1 in flight | 24,000 of 24,000, 197.3 tx/s | 2.0 / 2.9 / 3.0 s | 135.8 / 245.8 / 276.4 s | BatchesNotReachingL1 |
| 200 tx/s, 40 accounts | up to 6 blobs a transaction, 4 in flight | 24,000 of 24,000, 195.1 tx/s | 2.2 / 3.1 / 3.2 s | 21.3 / 28.6 / 34.6 s | none |

The sequencer kept up at 200 tx/s: inclusion stayed at 2 to 3 s with up to 401 of the
transactions in one 2 s block. The batcher did not. With one blob a transaction and one
transaction in flight, waiting 4 confirmations, it posts at most one blob about every
30 s. At 200 tx/s the L2 made data faster than that: up to 116 blocks waited to be
batched, the safe head fell up to 4.6 minutes behind, and BatchesNotReachingL1 fired,
as it should. The batcher now packs up to 6 blobs into a transaction and keeps up to 4
in flight ([devnet.md](devnet.md#l2-batcher)); the last row is the same load after that
change.

## Where it shows

During a run the dashboard's tiles stay green; "Gas used per L2 block" in Details:
execution rises, and "Blocks waiting to be batched" in Details: batcher shows the
batcher keeping up. If batches fell behind, BatchesNotReachingL1 would fire.

## Options

```sh
scripts/devnet/l2-load.sh --rate 200 --senders 40 --duration 3m
```

`--rate` must be at most 5 times `--senders`.

## Limits

- The tool, the chain and the monitoring share one computer. The numbers describe this
  devnet on that machine, not what OP Stack can do; a sequencer of its own and a load
  generator on another host would give higher, cleaner numbers.
- Transfers are the lightest load. Contract calls and larger calldata would fill blocks
  and batches sooner.
