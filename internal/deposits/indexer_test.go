package deposits

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/ledger/ledgertest"
)

var (
	vault = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	alice = common.HexToHash("0xa11ce")
	bob   = common.HexToHash("0xb0b")
)

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// fakeChain is a chain that can grow, reorganise and finalize on command. Logs stay
// readable by block hash after their block leaves the chain, as a node may serve them.
type fakeChain struct {
	mu        sync.Mutex
	blocks    []Header // index = number
	events    map[common.Hash][]Event
	finalized uint64
	salt      int
	failNext  error
}

// deposit is a deposit to put in a block: the same tx hash in two blocks is the same
// transaction included twice across a reorganisation.
type deposit struct {
	account common.Hash
	amount  *big.Int
	tx      common.Hash
	// A payment or a cancellation instead, of withdrawal id, to recipient.
	kind Kind
	id   common.Hash
	to   common.Address
}

func newFakeChain(n int) *fakeChain {
	c := &fakeChain{events: map[common.Hash][]Event{}}
	c.blocks = append(c.blocks, Header{Number: 0, Hash: crypto.Keccak256Hash([]byte("genesis"))})
	for range n {
		c.mine()
	}
	return c
}

func (c *fakeChain) mine(ds ...deposit) Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mineLocked(ds...)
}

func (c *fakeChain) mineLocked(ds ...deposit) Header {
	parent := c.blocks[len(c.blocks)-1]
	h := Header{Number: parent.Number + 1, Parent: parent.Hash,
		Hash: crypto.Keccak256Hash([]byte(fmt.Sprintf("block %d salt %d", parent.Number+1, c.salt)))}
	c.blocks = append(c.blocks, h)
	for i, d := range ds {
		e := Event{Kind: KindDeposit, Block: h.Number, BlockHash: h.Hash, TxHash: d.tx, LogIndex: uint(i),
			DepositID: big.NewInt(int64(h.Number*10 + uint64(i))), Token: Native, Sender: common.HexToAddress("0x5e"),
			Account: d.account, Amount: d.amount}
		switch d.kind {
		case KindPaid:
			e = Event{Kind: KindPaid, Block: h.Number, BlockHash: h.Hash, TxHash: d.tx, LogIndex: uint(i),
				WithdrawalID: d.id, Token: Native, Recipient: d.to, Amount: d.amount}
		case KindCancelled:
			e = Event{Kind: KindCancelled, Block: h.Number, BlockHash: h.Hash, TxHash: d.tx, LogIndex: uint(i), WithdrawalID: d.id}
		}
		c.events[h.Hash] = append(c.events[h.Hash], e)
	}
	return h
}

// reorg drops the top depth blocks and mines replacements with ds in the first one.
func (c *fakeChain) reorg(depth, replacements int, ds ...deposit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = c.blocks[:len(c.blocks)-depth]
	c.salt++
	for i := range replacements {
		if i == 0 {
			c.mineLocked(ds...)
		} else {
			c.mineLocked()
		}
	}
}

func (c *fakeChain) finalize(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finalized = n
}

func (c *fakeChain) err() error {
	err := c.failNext
	c.failNext = nil
	return err
}

func (c *fakeChain) Latest(context.Context) (Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.err(); err != nil {
		return Header{}, err
	}
	return c.blocks[len(c.blocks)-1], nil
}

func (c *fakeChain) Finalized(context.Context) (Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocks[c.finalized], nil
}

func (c *fakeChain) Header(_ context.Context, n uint64) (Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= uint64(len(c.blocks)) {
		return Header{}, fmt.Errorf("no block %d", n)
	}
	return c.blocks[n], nil
}

func (c *fakeChain) EventsInBlock(_ context.Context, _ common.Address, h common.Hash) ([]Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events[h]...), nil
}

func (c *fakeChain) EventsInRange(_ context.Context, _ common.Address, from, to uint64) ([]Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for n := from; n <= to && n < uint64(len(c.blocks)); n++ {
		out = append(out, c.events[c.blocks[n].Hash]...)
	}
	return out, nil
}

type fixture struct {
	chain *fakeChain
	l     *ledger.Ledger
	owner *pgx.Conn
}

func newFixture(t *testing.T, blocks int) *fixture {
	t.Helper()
	ctx := context.Background()
	db := ledgertest.New(t, ledger.Migrate, "deposit_indexer")
	l, err := ledger.Open(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	owner, err := pgx.Connect(ctx, db.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close(context.Background()) })
	return &fixture{chain: newFakeChain(blocks), l: l, owner: owner}
}

func (f *fixture) indexer() *Indexer {
	return &Indexer{Chain: f.chain, Ledger: f.l, Vault: vault, Start: 5, MaxRange: 8, MaxBlocks: 100}
}

func (f *fixture) step(t *testing.T) Report {
	t.Helper()
	rep, err := f.indexer().Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return rep
}

// balance checks what an account holds pending and posted, in ETH.
func (f *fixture) balance(t *testing.T, account common.Hash, pending, posted int64) {
	t.Helper()
	a, err := f.l.Account(context.Background(), CustomerAccount(account, "ETH"))
	if errors.Is(err, ledger.ErrNotFound) && pending == 0 && posted == 0 {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if a.CreditsPending.Cmp(eth(pending)) != 0 || a.CreditsPosted.Cmp(eth(posted)) != 0 {
		t.Fatalf("%s: pending %s, posted %s wei; want %d and %d ETH", a.Name, a.CreditsPending, a.CreditsPosted, pending, posted)
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

func (f *fixture) statuses(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), "SELECT status, count(*) FROM deposits GROUP BY status")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func tx(s string) common.Hash { return crypto.Keccak256Hash([]byte(s)) }

func TestStep_creditsADepositOnlyOnceFinal(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(3), tx: tx("a")})
	f.chain.mine()
	f.chain.finalize(4)
	rep := f.step(t)
	if rep.Seen != 1 || rep.Posted != 0 || rep.Tip != 7 {
		t.Fatalf("first step: %+v, want 1 seen, none posted, tip 7", rep)
	}
	f.balance(t, alice, 3, 0)

	f.chain.finalize(6)
	if rep := f.step(t); rep.Posted != 1 || rep.Seen != 0 {
		t.Fatalf("after finality: %+v, want 1 posted", rep)
	}
	f.balance(t, alice, 0, 3)
	if rep := f.step(t); rep.Posted != 0 || rep.Seen != 0 {
		t.Fatalf("a step with nothing new: %+v", rep)
	}
	f.balance(t, alice, 0, 3)
	f.clean(t)
}

func TestStep_aReorganisationVoidsAHeldDeposit(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine()
	f.chain.mine(deposit{account: alice, amount: eth(2), tx: tx("a")})
	f.chain.finalize(5)
	f.step(t)
	f.balance(t, alice, 2, 0)

	f.chain.reorg(2, 3) // blocks 6 and 7 replaced by three without the deposit
	rep := f.step(t)
	if rep.Fork == nil || *rep.Fork != 5 || rep.Voided != 1 || rep.Tip != 8 {
		t.Fatalf("after the reorganisation: %+v, want fork 5, 1 voided, tip 8", rep)
	}
	f.balance(t, alice, 0, 0)
	f.chain.finalize(8)
	f.step(t)
	f.balance(t, alice, 0, 0)
	if s := f.statuses(t); s["voided"] != 1 || len(s) != 1 {
		t.Fatalf("deposits by status: %v, want one voided", s)
	}
	f.clean(t)
}

func TestStep_aDepositMovedToAnotherBlockIsCreditedOnce(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(4), tx: tx("a")})
	f.step(t)
	f.balance(t, alice, 4, 0)

	f.chain.reorg(1, 2, deposit{account: alice, amount: eth(4), tx: tx("a")}) // the same transaction in the new block 6
	rep := f.step(t)
	if rep.Voided != 1 || rep.Seen != 1 {
		t.Fatalf("after the reorganisation: %+v, want the old one voided and the new one seen", rep)
	}
	f.balance(t, alice, 4, 0)
	f.chain.finalize(7)
	f.step(t)
	f.balance(t, alice, 0, 4)
	if s := f.statuses(t); s["voided"] != 1 || s["posted"] != 1 {
		t.Fatalf("deposits by status: %v, want one voided, one posted", s)
	}
	f.clean(t)
}

func TestStep_aShorterChainVoidsWhatItLost(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine()
	f.chain.mine()
	f.chain.mine(deposit{account: bob, amount: eth(1), tx: tx("b")})
	f.step(t)
	f.balance(t, bob, 1, 0)
	f.chain.reorg(3, 1) // head goes from 8 to 6
	rep := f.step(t)
	if rep.Fork == nil || *rep.Fork != 5 || rep.Voided != 1 || rep.Tip != 6 {
		t.Fatalf("after the chain got shorter: %+v, want fork 5, 1 voided, tip 6", rep)
	}
	f.balance(t, bob, 0, 0)
	f.clean(t)
}

func TestStep_finalBlocksAreReadInRanges(t *testing.T) {
	f := newFixture(t, 5)
	for i := range 40 {
		switch i {
		case 3:
			f.chain.mine(deposit{account: alice, amount: eth(1), tx: tx("a1")}, deposit{account: bob, amount: eth(2), tx: tx("b1")})
		case 25:
			f.chain.mine(deposit{account: alice, amount: eth(5), tx: tx("a2")})
		default:
			f.chain.mine()
		}
	}
	f.chain.finalize(40)
	rep := f.step(t)
	if rep.Seen != 3 || rep.Posted != 3 || rep.Tip != 45 {
		t.Fatalf("catching up: %+v, want 3 seen and posted, tip 45", rep)
	}
	f.balance(t, alice, 0, 6)
	f.balance(t, bob, 0, 2)
	var stored int
	if err := f.owner.QueryRow(context.Background(), "SELECT count(*) FROM indexed_blocks").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// The start block, five ranges of at most 8 final blocks, the 5 blocks not final.
	if stored != 1+5+5 {
		t.Fatalf("%d blocks stored, want 11", stored)
	}
	f.clean(t)
}

func TestStep_aFinalBlockLeavingTheChainStopsTheIndexer(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(3), tx: tx("a")})
	f.chain.mine()
	f.chain.finalize(6)
	f.step(t)
	f.balance(t, alice, 0, 3)
	f.chain.reorg(2, 3)
	f.chain.finalize(5)
	_, err := f.indexer().Step(context.Background())
	if !errors.Is(err, ErrFinalityViolation) {
		t.Fatalf("a final block replaced: %v, want ErrFinalityViolation", err)
	}
	f.balance(t, alice, 0, 3)
	f.clean(t)
}

func TestStep_aNodeErrorChangesNothing(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(1), tx: tx("a")})
	f.chain.failNext = errors.New("node down")
	if _, err := f.indexer().Step(context.Background()); err == nil {
		t.Fatal("a step with the node down succeeded")
	}
	f.balance(t, alice, 0, 0)
	f.step(t)
	f.balance(t, alice, 1, 0)
	f.clean(t)
}

// Two indexers on the same database, stepping at once while the chain grows,
// reorganises and finalizes, credit every deposit exactly once.
func TestStep_twoIndexersAtOnceCountEachDepositOnce(t *testing.T) {
	f := newFixture(t, 5)
	ctx := context.Background()
	want := map[common.Hash]int64{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := f.indexer()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := x.Step(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := range 60 {
		account := alice
		if i%3 == 0 {
			account = bob
		}
		f.chain.mine(deposit{account: account, amount: eth(1), tx: tx(fmt.Sprint(i))})
		want[account]++
		if i%10 == 9 {
			// The last deposit is dropped and comes back in the replacement block.
			f.chain.reorg(1, 1, deposit{account: account, amount: eth(1), tx: tx(fmt.Sprint(i))})
		}
		if i%5 == 4 {
			head, _ := f.chain.Latest(ctx)
			f.chain.finalize(head.Number - 2)
		}
	}
	head, _ := f.chain.Latest(ctx)
	f.chain.finalize(head.Number)
	for range 20 {
		if _, err := f.indexer().Step(ctx); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	f.balance(t, alice, 0, want[alice])
	f.balance(t, bob, 0, want[bob])
	f.clean(t)
}

func TestDecode_readsTheDepositedLog(t *testing.T) {
	id := common.BigToHash(big.NewInt(7))
	token := common.BytesToHash(Native.Bytes())
	from := common.BytesToHash(common.HexToAddress("0x5e").Bytes())
	data := append(alice.Bytes(), common.BigToHash(eth(2)).Bytes()...)
	l := types.Log{Topics: []common.Hash{TopicDeposited, id, token, from}, Data: data, BlockNumber: 9, Index: 1}
	e, err := Decode(l)
	if err != nil {
		t.Fatal(err)
	}
	if e.DepositID.Int64() != 7 || e.Token != Native || e.Account != alice || e.Amount.Cmp(eth(2)) != 0 || e.Block != 9 || e.LogIndex != 1 {
		t.Fatalf("decoded %+v", e)
	}
	for name, bad := range map[string]types.Log{
		"removed":    {Topics: l.Topics, Data: data, Removed: true},
		"topic":      {Topics: []common.Hash{id, id, token, from}, Data: data},
		"short data": {Topics: l.Topics, Data: data[:32]},
		"zero":       {Topics: l.Topics, Data: append(alice.Bytes(), make([]byte, 32)...)},
	} {
		if _, err := Decode(bad); err == nil {
			t.Fatalf("%s: decoded", name)
		}
	}
}

var recipient = common.HexToAddress("0xbeef")

// request records a withdrawal and its hold as internal/withdrawals does.
func (f *fixture) request(t *testing.T, id common.Hash, account common.Hash, n int64) {
	t.Helper()
	ctx := context.Background()
	v, err := f.l.CreateAccount(ctx, "vault:ETH", "ETH", ledger.CreditsWithinDebits)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.l.CreateAccount(ctx, CustomerAccount(account, "ETH"), "ETH", ledger.DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	key := "withdrawal:" + id.Hex()
	if _, err := f.owner.Exec(ctx, `INSERT INTO withdrawals (id, vault, request_key, account, token, recipient, amount, ledger_key, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, 'onchain')`, id.Bytes(), vault.Bytes(), id.Hex(), account.Bytes(),
		Native.Bytes(), recipient.Bytes(), eth(n).String(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.l.Transfer(ctx, ledger.Transfer{Key: key, Kind: ledger.Pending, Debit: c.ID, Credit: v.ID, Amount: eth(n), Code: "withdrawal"}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) withdrawalStatus(t *testing.T, id common.Hash) string {
	t.Helper()
	var s string
	if err := f.owner.QueryRow(context.Background(), "SELECT status FROM withdrawals WHERE id = $1", id.Bytes()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// owned is what the ledger owes the customer after holds: posted credits less debits.
func (f *fixture) owned(t *testing.T, name string, held, owned int64) {
	t.Helper()
	a, err := f.l.Account(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	got := new(big.Int).Sub(a.CreditsPosted, a.DebitsPosted)
	if a.DebitsPending.Cmp(eth(held)) != 0 || got.Cmp(eth(owned)) != 0 {
		t.Fatalf("%s: held %s, owned %s wei; want %d and %d ETH", name, a.DebitsPending, got, held, owned)
	}
}

// vaultHolds checks what the ledger holds for the vault: posted debits less credits.
func (f *fixture) vaultHolds(t *testing.T, n int64) {
	t.Helper()
	a, err := f.l.Account(context.Background(), "vault:ETH")
	if err != nil {
		t.Fatal(err)
	}
	if got := new(big.Int).Sub(a.DebitsPosted, a.CreditsPosted); got.Cmp(eth(n)) != 0 {
		t.Fatalf("the ledger holds %s wei for the vault, want %d ETH", got, n)
	}
}

func TestStep_aFinalPaymentSettlesItsWithdrawal(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(5), tx: tx("d")})
	f.chain.finalize(6)
	f.step(t)
	id := common.HexToHash("0x1d")
	f.request(t, id, alice, 2)
	f.owned(t, CustomerAccount(alice, "ETH"), 2, 5)

	f.chain.mine(deposit{kind: KindPaid, id: id, to: recipient, amount: eth(2), tx: tx("p")})
	if rep := f.step(t); rep.Settled != 0 {
		t.Fatalf("before finality: %+v, want nothing settled", rep)
	}
	f.owned(t, CustomerAccount(alice, "ETH"), 2, 5)
	f.chain.finalize(7)
	if rep := f.step(t); rep.Settled != 1 || rep.Suspense != 0 {
		t.Fatalf("after finality: %+v, want 1 settled", rep)
	}
	if s := f.withdrawalStatus(t, id); s != "settled" {
		t.Fatalf("withdrawal %s, want settled", s)
	}
	f.owned(t, CustomerAccount(alice, "ETH"), 0, 3)
	f.vaultHolds(t, 3)
	f.clean(t)
}

func TestStep_aPaymentNobodyRequestedGoesToSuspense(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(5), tx: tx("d")})
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0xbad"), to: recipient, amount: eth(1), tx: tx("p")})
	f.chain.finalize(7)
	if rep := f.step(t); rep.Suspense != 1 {
		t.Fatalf("step: %+v, want 1 payment to suspense", rep)
	}
	f.owned(t, CustomerAccount(alice, "ETH"), 0, 5)
	f.vaultHolds(t, 4) // as the vault on chain: 5 in, 1 out
	s, err := f.l.Account(context.Background(), SuspenseAccount("ETH"))
	if err != nil {
		t.Fatal(err)
	}
	if s.DebitsPosted.Cmp(eth(1)) != 0 {
		t.Fatalf("suspense holds %s wei, want 1 ETH", s.DebitsPosted)
	}
	f.clean(t)
}

func TestStep_aFinalCancellationReleasesTheHold(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(9), tx: tx("d")})
	f.chain.finalize(6)
	f.step(t)
	id := common.HexToHash("0x1d")
	f.request(t, id, alice, 6)
	f.chain.mine(deposit{kind: KindCancelled, id: id, tx: tx("c")})
	f.chain.finalize(7)
	if rep := f.step(t); rep.Cancelled != 1 {
		t.Fatalf("step: %+v, want 1 cancelled", rep)
	}
	if s := f.withdrawalStatus(t, id); s != "cancelled" {
		t.Fatalf("withdrawal %s, want cancelled", s)
	}
	f.owned(t, CustomerAccount(alice, "ETH"), 0, 9)
	f.vaultHolds(t, 9)
	f.clean(t)
}

func TestStep_aPaymentThatLeftTheChainSettlesNothing(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(5), tx: tx("d")})
	f.chain.finalize(6)
	f.step(t)
	id := common.HexToHash("0x1d")
	f.request(t, id, alice, 2)
	f.chain.mine(deposit{kind: KindPaid, id: id, to: recipient, amount: eth(2), tx: tx("p")})
	f.step(t)
	f.chain.reorg(1, 2) // the payment's block is gone
	f.chain.finalize(8)
	rep := f.step(t)
	if rep.Settled != 0 || rep.Fork == nil {
		t.Fatalf("after the reorganisation: %+v, want a fork and nothing settled", rep)
	}
	if s := f.withdrawalStatus(t, id); s != "onchain" {
		t.Fatalf("withdrawal %s, want it still open for the payer", s)
	}
	f.owned(t, CustomerAccount(alice, "ETH"), 2, 5)
	var voided int
	if err := f.owner.QueryRow(context.Background(), "SELECT count(*) FROM withdrawal_events WHERE status = 'voided'").Scan(&voided); err != nil || voided != 1 {
		t.Fatalf("%d payment events voided, %v; want 1", voided, err)
	}
	f.clean(t)
}

func TestDecode_readsPaymentsAndCancellations(t *testing.T) {
	id := common.HexToHash("0x1d")
	paid := types.Log{Topics: []common.Hash{TopicPaid, id, common.BytesToHash(Native.Bytes()), common.BytesToHash(recipient.Bytes())},
		Data: common.BigToHash(eth(2)).Bytes(), BlockNumber: 9}
	e, err := Decode(paid)
	if err != nil || e.Kind != KindPaid || e.WithdrawalID != id || e.Recipient != recipient || e.Amount.Cmp(eth(2)) != 0 {
		t.Fatalf("paid: %+v, %v", e, err)
	}
	cancelled := types.Log{Topics: []common.Hash{TopicCancelled, id, common.BytesToHash(common.HexToAddress("0x9").Bytes())}, BlockNumber: 9}
	e, err = Decode(cancelled)
	if err != nil || e.Kind != KindCancelled || e.WithdrawalID != id {
		t.Fatalf("cancelled: %+v, %v", e, err)
	}
	if _, err := Decode(types.Log{Topics: []common.Hash{TopicPaid, id}, Data: paid.Data}); err == nil {
		t.Fatal("a short WithdrawalPaid decoded")
	}
}

// A version that read only deposits, then one that reads withdrawals too, from the
// block after the tip it found: the final blocks below are backfilled for withdrawals
// alone, the ones not yet final once they are, and every payment is applied once.
func TestStep_aStreamAddedLaterIsBackfilledOnce(t *testing.T) {
	f := newFixture(t, 5)
	ctx := context.Background()
	f.chain.mine(deposit{account: alice, amount: eth(9), tx: tx("d")})                                              // 6
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0x1"), to: recipient, amount: eth(1), tx: tx("p1")}) // 7
	f.chain.mine()                                                                                                  // 8
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0x2"), to: recipient, amount: eth(2), tx: tx("p2")}) // 9
	f.chain.mine()                                                                                                  // 10
	f.chain.finalize(7)
	old := f.indexer()
	old.only = []string{StreamDeposits}
	if rep, err := old.Step(ctx); err != nil || rep.Suspense != 0 || rep.Tip != 10 {
		t.Fatalf("the old version: %+v, %v; want deposits only, tip 10", rep, err)
	}
	if _, err := f.l.Account(ctx, SuspenseAccount("ETH")); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("the old version touched suspense: %v", err)
	}

	// What migration 0005 does for a vault already read.
	if _, err := f.owner.Exec(ctx, `INSERT INTO indexer_streams (vault, stream, live_from, backfilled_to)
		SELECT vault, 'withdrawals', max(number) + 1, min(number) FROM indexed_blocks GROUP BY vault`); err != nil {
		t.Fatal(err)
	}
	rep := f.step(t)
	if rep.Suspense != 1 || rep.Backlog != 3 {
		t.Fatalf("first step of the new version: %+v, want the final payment applied and blocks 8..10 to wait", rep)
	}
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0x3"), to: recipient, amount: eth(3), tx: tx("p3")}) // 11, live
	if rep := f.step(t); rep.Suspense != 0 {
		t.Fatalf("a payment not yet final: %+v, want nothing applied", rep)
	}
	f.chain.finalize(11)
	rep = f.step(t)
	if rep.Suspense != 2 || rep.Backlog != 0 {
		t.Fatalf("after finality: %+v, want the backfilled and the live payment applied, nothing left", rep)
	}
	if rep := f.step(t); rep.Suspense != 0 || rep.Backfilled != 0 {
		t.Fatalf("a step with nothing new: %+v", rep)
	}
	s, err := f.l.Account(ctx, SuspenseAccount("ETH"))
	if err != nil {
		t.Fatal(err)
	}
	if s.DebitsPosted.Cmp(eth(6)) != 0 {
		t.Fatalf("suspense holds %s wei, want 6 ETH: each payment once", s.DebitsPosted)
	}
	f.vaultHolds(t, 3)
	f.clean(t)
}

// A stream with no starting block after blocks were read is refused, not read from the
// start again, which would count what was read twice.
func TestStep_aStreamWithoutAStartIsRefused(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(1), tx: tx("d")})
	old := f.indexer()
	old.only = []string{StreamDeposits}
	if _, err := old.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.indexer().Step(context.Background()); err == nil {
		t.Fatal("a stream added without a starting block was read")
	}
}

// An indexer of a version that does not read withdrawals, still running after the
// migration that added them: the database refuses the blocks it would store, so no
// block above the stream's start is read without it.
func TestStep_anOlderIndexerCannotWriteAfterTheMigration(t *testing.T) {
	f := newFixture(t, 5)
	ctx := context.Background()
	f.chain.mine(deposit{account: alice, amount: eth(1), tx: tx("d")})
	old := f.indexer()
	old.only = []string{StreamDeposits}
	if _, err := old.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, `INSERT INTO indexer_streams (vault, stream, live_from, backfilled_to)
		SELECT vault, 'withdrawals', max(number) + 1, min(number) FROM indexed_blocks GROUP BY vault`); err != nil {
		t.Fatal(err)
	}
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0x1"), to: recipient, amount: eth(1), tx: tx("p")})
	_, err := old.Step(ctx)
	if err == nil || !strings.Contains(err.Error(), "an older indexer is still running") {
		t.Fatalf("the older indexer after the migration: %v, want it refused", err)
	}
	f.chain.finalize(7)
	if rep := f.step(t); rep.Suspense != 1 {
		t.Fatalf("the new indexer: %+v, want the payment the older one could not store applied", rep)
	}
	f.clean(t)
}

// A deposit held while not final, then the indexer falls behind and catches up over
// final blocks in which the deposit is paid out: the deposit is posted before the
// payment is applied, as on chain.
func TestStep_eventsApplyInChainOrderWhenCatchingUp(t *testing.T) {
	f := newFixture(t, 5)
	f.chain.mine(deposit{account: alice, amount: eth(2), tx: tx("d")})
	f.step(t)
	f.balance(t, alice, 2, 0)
	for range 10 {
		f.chain.mine()
	}
	f.chain.mine(deposit{kind: KindPaid, id: common.HexToHash("0x1"), to: recipient, amount: eth(2), tx: tx("p")})
	f.chain.finalize(17)
	rep := f.step(t)
	if rep.Posted != 1 || rep.Suspense != 1 {
		t.Fatalf("catching up: %+v, want the deposit posted and the payment applied", rep)
	}
	f.vaultHolds(t, 0)
	f.clean(t)
}
