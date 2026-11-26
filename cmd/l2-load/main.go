// Command l2-load sends plain transfers to the L2 sequencer at a fixed rate and measures
// what the chain does with them: how many are included and how fast, and how long until
// the block that holds each one is safe, that is, derived from batches on the L1.
//
// The load comes from accounts of the public devnet mnemonic (read from
// SETTLED_DEVNET_MNEMONIC, never printed) at index 100 and up, which nothing else uses.
// They are topped up on the L2 from the devnet user first. Each transaction sends 0 wei
// to its own sender, so only fees are spent. The run fails unless every transaction is
// sent, included and successful, the included rate reaches 95% of the target, and every
// block that holds one becomes safe.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/loadstat"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// UserIndex is the devnet user, who holds ETH on the L2 from make l2-deposit and
	// funds the load accounts.
	UserIndex = 2
	// FirstSenderIndex is the first load account on the devnet mnemonic.
	FirstSenderIndex = 100
	transferGas      = 21_000
	// MinRatePercent is the share of the target rate the included rate must reach.
	MinRatePercent = 95
)

// record is one transaction of the run, as the sender and the watcher see it.
type record struct {
	sent     time.Time
	included time.Time // zero until its block is seen
	block    uint64
	ok       bool
}

// run state shared between senders and watchers.
type state struct {
	mu      sync.Mutex
	txs     map[common.Hash]*record
	blocks  map[uint64]time.Time // first seen
	safe    map[uint64]time.Time // first seen safe
	perBlk  map[uint64]int
	gasBlk  map[uint64]uint64
	sendErr []error
}

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	nodeURL := flag.String("node", "http://127.0.0.1:19545", "sequencer op-node RPC")
	rate := flag.Int("rate", 50, "transactions per second to send")
	duration := flag.Duration("duration", 2*time.Minute, "how long to send")
	senders := flag.Int("senders", 16, "accounts to send from; keep rate/senders at 5 or less")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *rpcURL, *nodeURL, *rate, *duration, *senders); err != nil {
		fmt.Fprintf(os.Stderr, "l2-load: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("l2-load: ok")
}

func run(ctx context.Context, rpcURL, nodeURL string, rate int, duration time.Duration, senders int) error {
	if rate <= 0 || senders <= 0 || duration <= 0 {
		return errors.New("rate, senders and duration must be positive")
	}
	if rate > 5*senders {
		return fmt.Errorf("%d tx/s from %d accounts is more than 5 per account per second; add --senders", rate, senders)
	}
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	raw, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer raw.Close()
	client := ethclient.NewClient(raw)
	node, err := rpc.DialContext(ctx, nodeURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", nodeURL, err)
	}
	defer node.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read chain id: %w", err)
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return fmt.Errorf("read gas tip: %w", err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("read head: %w", err)
	}
	if head.BaseFee == nil {
		return errors.New("head header has no base fee")
	}
	// Four times the base fee leaves room for it to rise under load during the run.
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(4)), tip)

	total := rate * int(duration.Seconds())
	perSender := (total + senders - 1) / senders
	// Each account needs its execution fees at the fee cap, doubled for the L1 data fee
	// and any rise; at least 0.01 ETH.
	need := new(big.Int).Mul(feeCap, big.NewInt(int64(transferGas*perSender*2)))
	if floor := new(big.Int).Div(funding.Ether(1), big.NewInt(100)); need.Cmp(floor) < 0 {
		need = floor
	}

	keys := make([]*ecdsa.PrivateKey, senders)
	targets := make([]funding.Target, senders)
	for i := range senders {
		k, err := devkeys.FromMnemonic(mnemonic, uint32(FirstSenderIndex+i))
		if err != nil {
			return fmt.Errorf("derive load account %d: %w", i, err)
		}
		keys[i] = k
		targets[i] = funding.Target{Name: fmt.Sprintf("load-%d", i), Address: crypto.PubkeyToAddress(k.PublicKey), Balance: need}
	}
	user, err := devkeys.FromMnemonic(mnemonic, UserIndex)
	if err != nil {
		return fmt.Errorf("derive the devnet user key: %w", err)
	}
	userAddr := crypto.PubkeyToAddress(user.PublicKey)
	userBalance, err := client.BalanceAt(ctx, userAddr, nil)
	if err != nil {
		return fmt.Errorf("read user balance: %w", err)
	}
	fmt.Printf("plan             %d tx/s for %s from %d accounts: %d transactions\n", rate, duration, senders, total)
	fmt.Printf("fees             base fee %s wei, tip %s wei; each account topped up to %s ETH\n", head.BaseFee, tip, funding.FormatEther(need))
	fmt.Printf("user             %s holds %s ETH on the L2\n", userAddr, funding.FormatEther(userBalance))

	fundCtx, cancelFund := context.WithTimeout(ctx, 2*time.Minute)
	results, err := funding.TopUp(fundCtx, client, user, targets)
	cancelFund()
	if err != nil {
		return fmt.Errorf("top up the load accounts (make l2-deposit adds ETH to the user): %w", err)
	}
	topped := 0
	for _, r := range results {
		if r.Sent.Sign() > 0 {
			topped++
		}
	}
	fmt.Printf("funded           %d of %d accounts topped up\n", topped, senders)

	nonces := make([]uint64, senders)
	for i, t := range targets {
		pending, err := client.PendingNonceAt(ctx, t.Address)
		if err != nil {
			return fmt.Errorf("read nonce of %s: %w", t.Name, err)
		}
		latest, err := client.NonceAt(ctx, t.Address, nil)
		if err != nil {
			return fmt.Errorf("read nonce of %s: %w", t.Name, err)
		}
		if pending != latest {
			return fmt.Errorf("%s has %d transactions still pending; wait for them and run again", t.Name, pending-latest)
		}
		nonces[i] = pending
	}

	start, err := client.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("read block number: %w", err)
	}
	st := &state{
		txs:    make(map[common.Hash]*record, total),
		blocks: map[uint64]time.Time{},
		safe:   map[uint64]time.Time{},
		perBlk: map[uint64]int{},
		gasBlk: map[uint64]uint64{},
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	watchErr := make(chan error, 2)
	go func() { watchErr <- watchBlocks(watchCtx, client, raw, start+1, st) }()
	go func() { watchErr <- watchSafe(watchCtx, node, st) }()

	// Senders. Each sends every senders/rate seconds, started at even offsets so the
	// load is spread over each second.
	interval := time.Duration(float64(time.Second) * float64(senders) / float64(rate))
	signer := types.LatestSignerForChainID(chainID)
	began := time.Now()
	deadline := began.Add(duration)
	var wg sync.WaitGroup
	for i := range senders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := targets[i].Address
			nonce := nonces[i]
			first := began.Add(interval * time.Duration(i) / time.Duration(senders))
			for n := 0; n < perSender; n++ {
				at := first.Add(interval * time.Duration(n))
				if !at.Before(deadline) || sentCount(st) >= total {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(at)):
				}
				tx, err := types.SignNewTx(keys[i], signer, &types.DynamicFeeTx{
					ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
					Gas: transferGas, To: &addr, Value: new(big.Int),
				})
				if err != nil {
					st.fail(fmt.Errorf("%s: sign: %w", targets[i].Name, err))
					return
				}
				st.mu.Lock()
				st.txs[tx.Hash()] = &record{sent: time.Now()}
				st.mu.Unlock()
				if err := client.SendTransaction(ctx, tx); err != nil {
					st.mu.Lock()
					delete(st.txs, tx.Hash())
					st.mu.Unlock()
					st.fail(fmt.Errorf("%s nonce %d: send: %w", targets[i].Name, nonce, err))
					return // a gap in nonces would hold every later one back
				}
				nonce++
			}
		}(i)
	}
	wg.Wait()
	sentSpan := time.Since(began)
	sent := sentCount(st)
	fmt.Printf("sent             %d in %.1f s (%.1f tx/s); send errors %d\n", sent, sentSpan.Seconds(), loadstat.Rate(sent, sentSpan), len(st.sendErr))

	if err := waitFor(ctx, watchErr, 2*time.Minute, func() bool { return st.allIncluded() }); err != nil {
		return fmt.Errorf("waiting for inclusion: %w", err)
	}
	if err := waitFor(ctx, watchErr, 8*time.Minute, func() bool { return st.allSafe() }); err != nil {
		return fmt.Errorf("waiting for the blocks to become safe: %w", err)
	}
	stopWatch()
	return report(st, rate, began)
}

func (s *state) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendErr = append(s.sendErr, err)
}

func sentCount(s *state) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.txs)
}

func (s *state) allIncluded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.txs {
		if r.included.IsZero() {
			return false
		}
	}
	return true
}

func (s *state) allSafe() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.txs {
		if _, ok := s.safe[r.block]; !ok {
			return false
		}
	}
	return true
}

// waitFor polls done until it holds, a watcher fails or the limit passes.
func waitFor(ctx context.Context, watchErr <-chan error, limit time.Duration, done func() bool) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	end := time.After(limit)
	for !done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-watchErr:
			return err
		case <-end:
			return fmt.Errorf("not done after %s", limit)
		case <-t.C:
		}
	}
	return nil
}

// blockReceipt is the part of a receipt the watcher needs. Decoding only these fields
// keeps OP Stack additions to the receipt, and the deposit transaction type, out of the
// way.
type blockReceipt struct {
	TxHash            common.Hash    `json:"transactionHash"`
	Status            hexutil.Uint64 `json:"status"`
	CumulativeGasUsed hexutil.Uint64 `json:"cumulativeGasUsed"`
}

// watchBlocks reads every new block's receipts and marks the run's transactions in it
// as included, with the time the block was first seen.
func watchBlocks(ctx context.Context, client *ethclient.Client, raw *rpc.Client, next uint64, st *state) error {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		head, err := client.BlockNumber(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read block number: %w", err)
		}
		for ; next <= head; next++ {
			var receipts []blockReceipt
			if err := raw.CallContext(ctx, &receipts, "eth_getBlockReceipts", hexutil.EncodeUint64(next)); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("read receipts of block %d: %w", next, err)
			}
			now := time.Now()
			st.mu.Lock()
			st.blocks[next] = now
			if len(receipts) > 0 {
				st.gasBlk[next] = uint64(receipts[len(receipts)-1].CumulativeGasUsed)
			}
			for _, r := range receipts {
				rec, ok := st.txs[r.TxHash]
				if !ok {
					continue
				}
				rec.included, rec.block, rec.ok = now, next, r.Status == 1
				st.perBlk[next]++
			}
			st.mu.Unlock()
		}
	}
}

// watchSafe follows the sequencer's safe head and marks each block with the time it was
// first seen safe.
func watchSafe(ctx context.Context, node *rpc.Client, st *state) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		var status struct {
			SafeL2 struct {
				Number uint64 `json:"number"`
			} `json:"safe_l2"`
		}
		if err := node.CallContext(ctx, &status, "optimism_syncStatus"); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read sync status: %w", err)
		}
		now := time.Now()
		st.mu.Lock()
		for b := range st.blocks {
			if _, ok := st.safe[b]; !ok && b <= status.SafeL2.Number {
				st.safe[b] = now
			}
		}
		st.mu.Unlock()
	}
}

func report(st *state, rate int, began time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var inclusion, safety []time.Duration
	var last time.Time
	failed := 0
	first, lastBlock := ^uint64(0), uint64(0)
	for _, r := range st.txs {
		if !r.ok {
			failed++
		}
		inclusion = append(inclusion, r.included.Sub(r.sent))
		safety = append(safety, st.safe[r.block].Sub(r.sent))
		if r.included.After(last) {
			last = r.included
		}
		first, lastBlock = min(first, r.block), max(lastBlock, r.block)
	}
	maxPer, maxGas := 0, uint64(0)
	for b, n := range st.perBlk {
		maxPer = max(maxPer, n)
		maxGas = max(maxGas, st.gasBlk[b])
	}
	included := len(st.txs)
	rateIncluded := loadstat.Rate(included, last.Sub(began))
	fmt.Printf("included         %d, %d failed, in blocks %d to %d; %.1f tx/s from first send to last inclusion\n", included, failed, first, lastBlock, rateIncluded)
	fmt.Printf("per block        at most %d of these transactions, at most %d gas used\n", maxPer, maxGas)
	fmt.Printf("inclusion        %s (send to block seen)\n", loadstat.Summarize(inclusion))
	fmt.Printf("safe             %s (send to block safe from the L1)\n", loadstat.Summarize(safety))

	switch {
	case len(st.sendErr) > 0:
		return fmt.Errorf("%d send errors, first: %w", len(st.sendErr), st.sendErr[0])
	case failed > 0:
		return fmt.Errorf("%d transactions failed", failed)
	case rateIncluded*100 < float64(rate*MinRatePercent):
		return fmt.Errorf("included %.1f tx/s, below %d%% of %d", rateIncluded, MinRatePercent, rate)
	}
	return nil
}
