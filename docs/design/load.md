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
