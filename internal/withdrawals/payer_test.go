package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/ledger/ledgertest"
)

var vault = common.HexToAddress("0x00000000000000000000000000000000000000aa")

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// fakeVault is a node with the vault on it: it keeps a mempool, mines on command in
// nonce order, and applies the vault's rule that an id is paid once. Large amounts are
// queued until their release block.
type fakeVault struct {
	mu        sync.Mutex
	chainID   *big.Int
	block     uint64
	nonce     map[common.Address]uint64
	mempool   map[common.Hash]*types.Transaction
	receipts  map[common.Hash]*Receipt
	records   map[common.Hash]OnChain
	payments  map[common.Hash]int // successful payments per id
	refuse    map[common.Hash]string
	large     *big.Int
	delay     uint64
	sendError error
	// nonces holds each account's nonce after each block; the finalized block trails
	// the head by finalDepth.
	nonces     map[uint64]map[common.Address]uint64
	finalDepth uint64
	// tip and feeCap are what the network asks; a transaction tipping less than minTip
	// is kept in the mempool and not mined.
	tip, feeCap, minTip *big.Int
}

func newFakeVault() *fakeVault {
	return &fakeVault{chainID: big.NewInt(901), block: 100, nonce: map[common.Address]uint64{},
		mempool: map[common.Hash]*types.Transaction{}, receipts: map[common.Hash]*Receipt{},
		records: map[common.Hash]OnChain{}, payments: map[common.Hash]int{}, refuse: map[common.Hash]string{},
		large: eth(5), delay: 10, nonces: map[uint64]map[common.Address]uint64{}, finalDepth: 3,
		tip: big.NewInt(1), feeCap: big.NewInt(1_000_000_000), minTip: big.NewInt(0)}
}

func (f *fakeVault) ChainID(context.Context) (*big.Int, error) { return f.chainID, nil }

func (f *fakeVault) Head(context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.block, nil
}

func (f *fakeVault) Withdrawal(_ context.Context, _ common.Address, id common.Hash) (OnChain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[id], nil
}

// apply runs a call against the vault's state at block; with commit it changes it.
func (f *fakeVault) apply(data []byte, block uint64, commit bool) error {
	m, err := VaultABI.MethodById(data[:4])
	if err != nil {
		return err
	}
	args, err := m.Inputs.Unpack(data[4:])
	if err != nil {
		return err
	}
	id := common.Hash(args[0].([32]byte))
	if name := f.refuse[id]; name != "" {
		return &Revert{Name: name}
	}
	rec := f.records[id]
	switch m.Name {
	case "withdraw":
		if rec.Status != None {
			return &Revert{Name: "WithdrawalExists"}
		}
		if commit {
			if args[3].(*big.Int).Cmp(f.large) >= 0 {
				f.records[id] = OnChain{Status: Queued, ReleaseAt: block + f.delay}
			} else {
				f.records[id] = OnChain{Status: Paid}
				f.payments[id]++
			}
		}
	case "executeQueued":
		if rec.Status != Queued {
			return &Revert{Name: "WithdrawalNotQueued"}
		}
		if block < rec.ReleaseAt {
			return &Revert{Name: "WithdrawalNotReleased"}
		}
		if commit {
			f.records[id] = OnChain{Status: Paid}
			f.payments[id]++
		}
	}
	return nil
}

func (f *fakeVault) Estimate(_ context.Context, _, _ common.Address, data []byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.apply(data, f.block+1, false); err != nil {
		return 0, err
	}
	return 60_000, nil
}

func (f *fakeVault) Nonce(_ context.Context, a common.Address) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nonce[a], nil
}

func (f *fakeVault) Finalized(_ context.Context, a common.Address) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block < 100+f.finalDepth {
		return 100, 0, nil
	}
	b := f.block - f.finalDepth
	return b, f.nonces[b][a], nil
}

func (f *fakeVault) Fees(context.Context) (*big.Int, *big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tip, f.feeCap, nil
}

func (f *fakeVault) Send(_ context.Context, tx *types.Transaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendError != nil {
		return f.sendError
	}
	if _, ok := f.receipts[tx.Hash()]; ok {
		return errors.New("already known")
	}
	if _, ok := f.mempool[tx.Hash()]; ok {
		return errors.New("already known")
	}
	// A transaction under a nonce already in the mempool replaces it, as nodes do, only
	// if it pays at least a tenth more.
	signer := types.LatestSignerForChainID(f.chainID)
	from, _ := types.Sender(signer, tx)
	for h, old := range f.mempool {
		if o, _ := types.Sender(signer, old); o != from || old.Nonce() != tx.Nonce() {
			continue
		}
		if !atLeastATenthMore(tx.GasTipCap(), old.GasTipCap()) || !atLeastATenthMore(tx.GasFeeCap(), old.GasFeeCap()) {
			return errors.New("replacement transaction underpriced")
		}
		delete(f.mempool, h)
	}
	f.mempool[tx.Hash()] = tx
	return nil
}

func (f *fakeVault) Receipt(_ context.Context, h common.Hash) (*Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receipts[h], nil
}

// mine makes one block of every transaction in the mempool that fits the sender's
// nonce, in nonce order; a failed call is mined too and uses its nonce.
func (f *fakeVault) mine() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block++
	signer := types.LatestSignerForChainID(f.chainID)
	for {
		progressed := false
		txs := make([]*types.Transaction, 0, len(f.mempool))
		for _, t := range f.mempool {
			txs = append(txs, t)
		}
		sort.Slice(txs, func(i, j int) bool { return txs[i].Nonce() < txs[j].Nonce() })
		for _, t := range txs {
			from, _ := types.Sender(signer, t)
			if t.Nonce() != f.nonce[from] || t.GasTipCap().Cmp(f.minTip) < 0 {
				continue
			}
			err := f.apply(t.Data(), f.block, true)
			f.receipts[t.Hash()] = &Receipt{OK: err == nil, Block: f.block}
			f.nonce[from]++
			delete(f.mempool, t.Hash())
			progressed = true
		}
		if !progressed {
			f.nonces[f.block] = clone(f.nonce)
			return
		}
	}
}

type snapshot struct {
	block    uint64
	nonce    map[common.Address]uint64
	receipts map[common.Hash]*Receipt
	records  map[common.Hash]OnChain
	payments map[common.Hash]int
	nonces   map[uint64]map[common.Address]uint64
}

func clone[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// snap keeps the state before a block, so reorg can take it back.
func (f *fakeVault) snap() snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return snapshot{f.block, clone(f.nonce), clone(f.receipts), clone(f.records), clone(f.payments), clone(f.nonces)}
}

// reorg puts the state back to s: the blocks after it, their transactions, payments
// and nonces are gone, and so is the node's mempool.
func (f *fakeVault) reorg(s snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block, f.nonce, f.receipts, f.records, f.payments, f.nonces = s.block, clone(s.nonce), clone(s.receipts), clone(s.records), clone(s.payments), clone(s.nonces)
	f.mempool = map[common.Hash]*types.Transaction{}
}

type fixture struct {
	chain *fakeVault
	l     *ledger.Ledger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := ledgertest.New(t, ledger.Migrate, "withdrawal_requester, withdrawal_payer, deposit_indexer")
	l, err := ledger.Open(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return &fixture{chain: newFakeVault(), l: l}
}

func (f *fixture) payer(t *testing.T) *Payer {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("operator")))
	if err != nil {
		t.Fatal(err)
	}
	return &Payer{Chain: f.chain, Ledger: f.l, Vault: vault, Signer: KeySigner{Key: key}, ResendAfter: time.Hour, MaxFeeCap: big.NewInt(100_000_000_000), Batch: 100}
}

// fund credits a customer as a final deposit would.
func (f *fixture) fund(t *testing.T, account common.Hash, n int64) {
	t.Helper()
	ctx := context.Background()
	v, err := f.l.CreateAccount(ctx, "vault:ETH", "ETH", ledger.CreditsWithinDebits)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.l.CreateAccount(ctx, deposits.CustomerAccount(account, "ETH"), "ETH", ledger.DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Transfer(ctx, ledger.Transfer{Key: "deposit:" + account.Hex() + fmt.Sprint(n), Kind: ledger.Single, Debit: v.ID, Credit: c.ID, Amount: eth(n), Code: "deposit"}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) submit(t *testing.T, key string, account common.Hash, n int64) Withdrawal {
	t.Helper()
	w, err := Submit(context.Background(), f.l, vault, Request{Key: key, Account: account, Token: deposits.Native, Recipient: common.HexToAddress("0xbeef"), Amount: eth(n)})
	if err != nil {
		t.Fatalf("submit %s: %v", key, err)
	}
	return w
}

func (f *fixture) step(t *testing.T, p *Payer) Report {
	t.Helper()
	rep, err := p.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return rep
}

func (f *fixture) status(t *testing.T, id common.Hash) string {
	t.Helper()
	var s string
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT status FROM withdrawals WHERE id = $1", id.Bytes()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) balance(t *testing.T, account common.Hash, held, owned int64) {
	t.Helper()
	a, err := f.l.Account(context.Background(), deposits.CustomerAccount(account, "ETH"))
	if err != nil {
		t.Fatal(err)
	}
	gotOwned := new(big.Int).Sub(a.CreditsPosted, a.DebitsPosted)
	if a.DebitsPending.Cmp(eth(held)) != 0 || gotOwned.Cmp(eth(owned)) != 0 {
		t.Fatalf("%s: held %s, owned %s wei; want %d and %d ETH", a.Name, a.DebitsPending, gotOwned, held, owned)
	}
}

func (f *fixture) clean(t *testing.T) {
	t.Helper()
	problems, err := f.l.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("the books disagree with the journal: %v", problems)
	}
}

func (f *fixture) txCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT count(*) FROM withdrawal_txs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var alice = common.HexToHash("0xa11ce")

func TestSubmit_holdsTheBalanceOnce(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 2)
	again := f.submit(t, "w-1", alice, 2)
	if again.ID != w.ID || w.ID != ID(vault, "w-1") {
		t.Fatalf("the same request again is %s, want %s", again.ID, w.ID)
	}
	f.balance(t, alice, 2, 5)
	ctx := context.Background()
	if _, err := Submit(ctx, f.l, vault, Request{Key: "w-1", Account: alice, Token: deposits.Native, Recipient: common.HexToAddress("0xbeef"), Amount: eth(3)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key, other amount: %v, want ErrConflict", err)
	}
	if _, err := Submit(ctx, f.l, vault, Request{Key: "w-2", Account: alice, Token: deposits.Native, Recipient: common.HexToAddress("0xbeef"), Amount: eth(4)}); !errors.Is(err, ledger.ErrExceedsCredits) {
		t.Fatalf("more than the balance left: %v, want ErrExceedsCredits", err)
	}
	var n int
	if err := f.l.Pool().QueryRow(ctx, "SELECT count(*) FROM withdrawals").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d withdrawals recorded, %v; want 1", n, err)
	}
	f.clean(t)
}

func TestPayer_paysOnceAndThenOnlyWatches(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("first step: %+v, want 1 signed", rep)
	}
	if rep := f.step(t, p); rep.Signed != 0 {
		t.Fatalf("a step with the transaction in flight: %+v, want nothing signed", rep)
	}
	f.chain.mine()
	if rep := f.step(t, p); rep.OnChain != 1 || rep.Signed != 0 {
		t.Fatalf("after mining: %+v, want it on chain", rep)
	}
	for range 3 {
		f.chain.mine()
		f.step(t, p)
	}
	if f.chain.payments[w.ID] != 1 || f.txCount(t) != 1 || f.status(t, w.ID) != "onchain" {
		t.Fatalf("%d payments, %d transactions, status %s; want 1, 1, onchain", f.chain.payments[w.ID], f.txCount(t), f.status(t, w.ID))
	}
	f.balance(t, alice, 1, 5)
	f.clean(t)
}

func TestPayer_aCrashBeforeSendingSendsTheSameBytesLater(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	crash := errors.New("crash")
	var signed common.Hash
	p.beforeSend = func(tx *types.Transaction) error { signed = tx.Hash(); return crash }
	if _, err := p.Step(context.Background()); !errors.Is(err, crash) {
		t.Fatalf("step: %v, want the crash", err)
	}
	p.beforeSend = nil
	p.ResendAfter = 0
	if rep := f.step(t, p); rep.Resent != 1 || rep.Signed != 0 {
		t.Fatalf("after the crash: %+v, want the recorded transaction resent and nothing new signed", rep)
	}
	f.chain.mine()
	if r := f.chain.receipts[signed]; r == nil || !r.OK {
		t.Fatalf("the transaction recorded before the crash was not the one mined: %+v", r)
	}
	f.step(t, p)
	if f.chain.payments[w.ID] != 1 || f.txCount(t) != 1 || f.chain.nonce[p.sender()] != 1 {
		t.Fatalf("%d payments, %d transactions, nonce %d; want 1, 1, 1", f.chain.payments[w.ID], f.txCount(t), f.chain.nonce[p.sender()])
	}
}

func TestPayer_aDroppedTransactionIsSentAgain(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	f.chain.reorg(f.chain.snap()) // the node restarts and loses its mempool
	f.chain.mine()
	p.ResendAfter = 0
	if rep := f.step(t, p); rep.Resent != 1 {
		t.Fatalf("after the mempool was lost: %+v, want it resent", rep)
	}
	f.chain.mine()
	f.step(t, p)
	if f.chain.payments[w.ID] != 1 || f.status(t, w.ID) != "onchain" {
		t.Fatalf("%d payments, status %s; want 1, onchain", f.chain.payments[w.ID], f.status(t, w.ID))
	}
}

func TestPayer_aReorganisationIsPaidAgainOnce(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	before := f.chain.snap()
	f.chain.mine()
	f.step(t, p)
	if f.status(t, w.ID) != "onchain" {
		t.Fatal("not on chain after mining")
	}
	f.chain.reorg(before) // the block with the payment is gone
	f.chain.mine()
	p.ResendAfter = 0
	if rep := f.step(t, p); rep.Reopened != 1 || rep.Resent != 1 || rep.Signed != 0 {
		t.Fatalf("after the reorganisation: %+v, want it reopened and the same transaction resent", rep)
	}
	f.chain.mine()
	f.step(t, p)
	if f.chain.payments[w.ID] != 1 || f.txCount(t) != 1 || f.status(t, w.ID) != "onchain" {
		t.Fatalf("%d payments, %d transactions, status %s; want 1, 1, onchain", f.chain.payments[w.ID], f.txCount(t), f.status(t, w.ID))
	}
}

func TestPayer_aRefusalForGoodReleasesTheHold(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 2)
	f.chain.refuse[w.ID] = "NativeTransferFailed"
	p := f.payer(t)
	if rep := f.step(t, p); rep.Rejected != 1 || rep.Signed != 0 {
		t.Fatalf("step: %+v, want it rejected and nothing signed", rep)
	}
	if f.status(t, w.ID) != "rejected" {
		t.Fatalf("status %s, want rejected", f.status(t, w.ID))
	}
	f.balance(t, alice, 0, 5)
	f.clean(t)
}

func TestPayer_aRateLimitIsWaitedOut(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	f.chain.refuse[w.ID] = "RateLimited"
	p := f.payer(t)
	if rep := f.step(t, p); rep.Waiting != 1 || rep.Signed != 0 {
		t.Fatalf("rate limited: %+v, want it waiting", rep)
	}
	var lastErr string
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT coalesce(last_error, '') FROM withdrawals WHERE id = $1", w.ID.Bytes()).Scan(&lastErr); err != nil || lastErr == "" {
		t.Fatalf("no last error recorded: %q, %v", lastErr, err)
	}
	delete(f.chain.refuse, w.ID)
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("after the limit refilled: %+v, want it signed", rep)
	}
	f.chain.mine()
	f.step(t, p)
	if f.chain.payments[w.ID] != 1 {
		t.Fatalf("%d payments, want 1", f.chain.payments[w.ID])
	}
}

func TestPayer_aQueuedWithdrawalIsPaidAfterItsDelay(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 10)
	w := f.submit(t, "w-1", alice, 6) // at or above the queue threshold
	p := f.payer(t)
	f.step(t, p)
	f.chain.mine()
	if rep := f.step(t, p); rep.OnChain != 1 || rep.Waiting != 1 {
		t.Fatalf("queued: %+v, want it on chain and waiting", rep)
	}
	for range 8 {
		f.chain.mine()
		if rep := f.step(t, p); rep.Signed != 0 {
			t.Fatalf("before the release block: %+v, want nothing signed", rep)
		}
	}
	f.chain.mine()
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("at the release block: %+v, want executeQueued signed", rep)
	}
	f.chain.mine()
	f.step(t, p)
	if f.chain.payments[w.ID] != 1 || f.txCount(t) != 2 {
		t.Fatalf("%d payments, %d transactions; want 1 and 2 (withdraw, execute)", f.chain.payments[w.ID], f.txCount(t))
	}
}

// Several payers with the same key, at once, while blocks are mined: every withdrawal
// is paid once, and no nonce is used twice or skipped.
func TestPayer_payersAtOnceWithOneKeyPayEachOnce(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 100)
	var ids []common.Hash
	for i := range 30 {
		ids = append(ids, f.submit(t, fmt.Sprintf("w-%d", i), alice, 1).ID)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	stop := make(chan struct{})
	for range 4 {
		p := f.payer(t)
		p.ResendAfter = 0
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := p.Step(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for range 40 {
		f.chain.mine()
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	p := f.payer(t)
	for range 3 {
		f.chain.mine()
		f.step(t, p)
	}
	for _, id := range ids {
		if f.chain.payments[id] != 1 || f.status(t, id) != "onchain" {
			t.Fatalf("withdrawal %s: %d payments, status %s; want 1, onchain", id, f.chain.payments[id], f.status(t, id))
		}
	}
	var txs, distinct int
	if err := f.l.Pool().QueryRow(ctx, "SELECT count(*), count(DISTINCT nonce) FROM withdrawal_txs").Scan(&txs, &distinct); err != nil {
		t.Fatal(err)
	}
	if txs != distinct || uint64(txs) != f.chain.nonce[p.sender()] {
		t.Fatalf("%d transactions, %d nonces, account nonce %d: want all equal", txs, distinct, f.chain.nonce[p.sender()])
	}
	f.balance(t, alice, 30, 100)
	f.clean(t)
}

// The network's fees rise above what a sent transaction pays: it is not mined. Once it
// has waited BumpAfter it is replaced under the same nonce at the network's fees, the
// replacement is mined, and the withdrawal is paid once with one nonce used.
func TestPayer_aStuckTransactionIsReplacedAndPaidOnce(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	p.BumpAfter = time.Hour
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("first step: %+v, want 1 signed", rep)
	}
	f.chain.tip, f.chain.minTip = big.NewInt(5), big.NewInt(5)
	f.chain.mine()
	if rep := f.step(t, p); rep.Bumped != 0 || rep.Signed != 0 {
		t.Fatalf("before BumpAfter: %+v, want nothing replaced or signed", rep)
	}
	p.BumpAfter = time.Nanosecond
	if rep := f.step(t, p); rep.Bumped != 1 || rep.Signed != 0 {
		t.Fatalf("after BumpAfter: %+v, want it replaced", rep)
	}
	f.chain.mine()
	if rep := f.step(t, p); rep.OnChain != 1 || rep.Bumped != 0 {
		t.Fatalf("after mining the replacement: %+v, want it on chain and nothing replaced", rep)
	}
	for range 4 {
		f.chain.mine()
		f.step(t, p)
	}
	var versions, open, mined int
	var tip string
	if err := f.l.Pool().QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE NOT done),
		count(DISTINCT nonce), max(tip_cap)::text FROM withdrawal_txs`).Scan(&versions, &open, &mined, &tip); err != nil {
		t.Fatal(err)
	}
	if f.chain.payments[w.ID] != 1 || versions != 2 || open != 0 || mined != 1 || tip != "5" || f.chain.nonce[p.sender()] != 1 {
		t.Fatalf("%d payments, %d versions (%d open) under %d nonces, top tip %s, account nonce %d; want 1, 2 (0), 1, 5, 1",
			f.chain.payments[w.ID], versions, open, mined, tip, f.chain.nonce[p.sender()])
	}
	f.balance(t, alice, 1, 5)
	f.clean(t)
}

// A replacement never offers more than MaxFeeCap: with no room for a tenth more, the
// transaction is left as it is, sent again when due, and the reason recorded.
func TestPayer_aReplacementStopsAtMaxFeeCap(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	p.MaxFeeCap = big.NewInt(1_050_000_000) // the first offers 1 gwei: a tenth more is above it
	f.step(t, p)
	f.chain.minTip = big.NewInt(5)
	f.chain.mine()
	p.BumpAfter, p.ResendAfter = time.Nanosecond, 0
	if rep := f.step(t, p); rep.Bumped != 0 || rep.Capped != 1 || rep.Resent != 1 {
		t.Fatalf("at the cap: %+v, want it capped and resent", rep)
	}
	var lastErr string
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT coalesce(last_error, '') FROM withdrawals WHERE id = $1", w.ID.Bytes()).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if lastErr == "" || f.txCount(t) != 1 {
		t.Fatalf("last error %q, %d transactions; want the reason and 1", lastErr, f.txCount(t))
	}
}

// The network asking more than MaxFeeCap is waited out: nothing is signed and no nonce
// is taken until it asks less.
func TestPayer_aNetworkAboveMaxFeeCapIsWaitedOut(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.chain.feeCap = new(big.Int).Add(p.MaxFeeCap, big.NewInt(1))
	if rep := f.step(t, p); rep.Signed != 0 || rep.Waiting != 1 || f.txCount(t) != 0 {
		t.Fatalf("above the cap: %+v, %d transactions; want it waiting and none", rep, f.txCount(t))
	}
	f.chain.feeCap = big.NewInt(1_000_000_000)
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("below the cap again: %+v, want it signed", rep)
	}
	f.chain.mine()
	f.step(t, p)
	var nonce uint64
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT nonce FROM withdrawal_txs").Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	if f.chain.payments[w.ID] != 1 || nonce != 0 {
		t.Fatalf("%d payments at nonce %d; want 1 at 0", f.chain.payments[w.ID], nonce)
	}
}

// Any version of a slot may be the one mined: the first is mined after its replacement
// was recorded but never reached the node. The slot closes once final, and nothing is
// paid or signed again.
func TestPayer_theFirstVersionMinedAfterAReplacementClosesTheSlot(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	var first common.Hash
	for h := range f.chain.mempool {
		first = h
	}
	crash := errors.New("crash")
	p.BumpAfter = time.Nanosecond
	p.beforeSend = func(*types.Transaction) error { return crash }
	if _, err := p.Step(context.Background()); !errors.Is(err, crash) {
		t.Fatalf("step: %v, want the crash", err)
	}
	p.beforeSend, p.BumpAfter = nil, 0
	f.chain.mine() // the node had only the first version
	if r := f.chain.receipts[first]; r == nil || !r.OK {
		t.Fatalf("the first version was not mined: %+v", r)
	}
	for range 5 {
		f.chain.mine()
		if rep := f.step(t, p); rep.Signed != 0 || rep.Bumped != 0 {
			t.Fatalf("after the first version was mined: %+v, want nothing signed", rep)
		}
	}
	var open int
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT count(*) FROM withdrawal_txs WHERE NOT done").Scan(&open); err != nil {
		t.Fatal(err)
	}
	if f.chain.payments[w.ID] != 1 || f.txCount(t) != 2 || open != 0 || f.chain.nonce[p.sender()] != 1 {
		t.Fatalf("%d payments, %d versions, %d open, nonce %d; want 1, 2, 0, 1", f.chain.payments[w.ID], f.txCount(t), open, f.chain.nonce[p.sender()])
	}
}

// The database holds the rules of the outbox, whatever the payer does.
func TestOutbox_theDatabaseRefusesWhatIsNotAReplacement(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	ctx := context.Background()
	var id int64
	var data []byte
	if err := f.l.Pool().QueryRow(ctx, "SELECT id, data FROM withdrawal_txs").Scan(&id, &data); err != nil {
		t.Fatal(err)
	}
	insert := func(hash byte, nonce int, tip, feeCap any, data []byte, replaces any) error {
		_, err := f.l.Pool().Exec(ctx, `INSERT INTO withdrawal_txs (withdrawal_id, kind, sender, nonce, hash, raw, tip_cap, fee_cap, data, replaces)
			VALUES ($1, 'withdraw', $2, $3, $4, '\x00', $5::numeric, $6::numeric, $7, $8)`,
			w.ID.Bytes(), p.sender().Bytes(), nonce, common.BytesToHash([]byte{hash}).Bytes(), tip, feeCap, data, replaces)
		return err
	}
	for _, c := range []struct {
		name, want string
		err        error
	}{
		{"no fees", "records its fees and call", insert(1, 1, nil, nil, nil, nil)},
		{"a second first version of the slot", "withdrawal_txs_first_of_slot", insert(2, 0, "2", "2000000000", data, nil)},
		{"less than a tenth more", "at least a tenth more", insert(3, 0, "1", "1050000000", data, id)},
		{"another call", "is not a replacement", insert(4, 0, "2", "2000000000", []byte{1}, id)},
		{"another nonce", "is not a replacement", insert(5, 1, "2", "2000000000", data, id)},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s: %v, want it refused: %s", c.name, c.err, c.want)
		}
	}
	if err := insert(6, 0, "2", "2000000000", data, id); err != nil {
		t.Fatalf("a replacement a tenth higher: %v", err)
	}
	if err := insert(7, 0, "3", "3000000000", data, id); err == nil || !strings.Contains(err.Error(), "withdrawal_txs_replaces_key") {
		t.Errorf("a second replacement of the same version: %v, want it refused", err)
	}
	if _, err := f.l.Pool().Exec(ctx, "UPDATE withdrawal_txs SET done = true WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Pool().Exec(ctx, "UPDATE withdrawal_txs SET done = false WHERE id = $1", id); err == nil || !strings.Contains(err.Error(), "done once set") {
		t.Errorf("a closed transaction reopened: %v, want it refused", err)
	}
}

// The indexer settles a withdrawal once its payment is final, before the payer looks
// again: the payer still closes its transaction, and signs nothing for it.
func TestPayer_closesTheTransactionsOfASettledWithdrawal(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	f.chain.mine()
	for range 4 {
		f.chain.mine()
	}
	ctx := context.Background()
	// As the indexer does once the payment is final.
	if _, err := f.l.Pool().Exec(ctx, "UPDATE withdrawals SET status = 'settled' WHERE id = $1", w.ID.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Transfer(ctx, ledger.Transfer{Key: ledgerKey(w.ID) + ":post", Kind: ledger.Post, PendingKey: ledgerKey(w.ID), Code: "withdrawal"}); err != nil {
		t.Fatal(err)
	}
	if rep := f.step(t, p); rep.Closed != 1 || rep.Signed != 0 || rep.Resent != 0 {
		t.Fatalf("after the indexer settled it: %+v, want its transaction closed and nothing sent", rep)
	}
	var open int
	if err := f.l.Pool().QueryRow(ctx, "SELECT count(*) FROM withdrawal_txs WHERE NOT done").Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 || f.chain.payments[w.ID] != 1 {
		t.Fatalf("%d transactions open, %d payments; want 0 and 1", open, f.chain.payments[w.ID])
	}
	f.balance(t, alice, 0, 4)
	f.clean(t)
}

// refusing is a signer whose policy refuses what refuse says, and signs the rest.
type refusing struct {
	KeySigner
	refuse func(*types.Transaction) bool
	asked  int
}

func (r *refusing) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	r.asked++
	if r.refuse(tx) {
		return nil, &Refused{Rule: "test", Reason: "not allowed"}
	}
	return r.KeySigner.Sign(ctx, tx)
}

// A signer's refusal is waited out: nothing is recorded, no nonce is taken, the reason
// is kept, and once the signer signs again the withdrawal is paid, under the next nonce.
func TestPayer_aSignersRefusalIsWaitedOutAndTakesNoNonce(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	w := f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	s := &refusing{KeySigner: p.Signer.(KeySigner), refuse: func(*types.Transaction) bool { return true }}
	p.Signer = s
	if rep := f.step(t, p); rep.Refused != 1 || rep.Waiting != 1 || rep.Signed != 0 || f.txCount(t) != 0 {
		t.Fatalf("refused: %+v, %d transactions; want it refused, waiting, nothing recorded", rep, f.txCount(t))
	}
	var lastErr string
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT coalesce(last_error, '') FROM withdrawals WHERE id = $1", w.ID.Bytes()).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastErr, "signer refused (test)") {
		t.Fatalf("last error %q, want the signer's refusal", lastErr)
	}
	s.refuse = func(*types.Transaction) bool { return false }
	if rep := f.step(t, p); rep.Signed != 1 {
		t.Fatalf("signed again: %+v, want it signed", rep)
	}
	f.chain.mine()
	f.step(t, p)
	var nonce uint64
	if err := f.l.Pool().QueryRow(context.Background(), "SELECT nonce FROM withdrawal_txs").Scan(&nonce); err != nil {
		t.Fatal(err)
	}
	if f.chain.payments[w.ID] != 1 || nonce != 0 {
		t.Fatalf("%d payments at nonce %d; want 1 at 0", f.chain.payments[w.ID], nonce)
	}
}

// A replacement the signer refuses leaves the transaction as it is, sent again when due.
func TestPayer_aRefusedReplacementKeepsTheTransaction(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	f.step(t, p)
	f.chain.minTip = big.NewInt(5)
	f.chain.mine()
	p.Signer = &refusing{KeySigner: p.Signer.(KeySigner), refuse: func(*types.Transaction) bool { return true }}
	p.BumpAfter, p.ResendAfter = time.Nanosecond, 0
	if rep := f.step(t, p); rep.Refused != 1 || rep.Bumped != 0 || rep.Resent != 1 || f.txCount(t) != 1 {
		t.Fatalf("a refused replacement: %+v, %d transactions; want it refused and the first sent again", rep, f.txCount(t))
	}
}

// liar returns a transaction other than the one asked for: the payer refuses to record it.
type liar struct{ KeySigner }

func (l liar) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	other := types.NewTx(&types.DynamicFeeTx{ChainID: tx.ChainId(), Nonce: tx.Nonce(), GasTipCap: tx.GasTipCap(), GasFeeCap: tx.GasFeeCap(),
		Gas: tx.Gas(), To: tx.To(), Value: big.NewInt(1), Data: tx.Data()})
	return l.KeySigner.Sign(ctx, other)
}

func TestPayer_aTransactionOtherThanTheOneAskedForIsNotRecorded(t *testing.T) {
	f := newFixture(t)
	f.fund(t, alice, 5)
	f.submit(t, "w-1", alice, 1)
	p := f.payer(t)
	p.Signer = liar{p.Signer.(KeySigner)}
	if _, err := p.Step(context.Background()); err == nil || !strings.Contains(err.Error(), "other than the one asked for") {
		t.Fatalf("step: %v, want it refused", err)
	}
	if f.txCount(t) != 0 || len(f.chain.mempool) != 0 {
		t.Fatalf("%d transactions recorded, %d sent; want none", f.txCount(t), len(f.chain.mempool))
	}
}
