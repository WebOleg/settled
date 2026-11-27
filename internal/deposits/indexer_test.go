package deposits

import (
	"context"
	"errors"
	"fmt"
	"math/big"
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
		c.events[h.Hash] = append(c.events[h.Hash], Event{
			Block: h.Number, BlockHash: h.Hash, TxHash: d.tx, LogIndex: uint(i),
			DepositID: big.NewInt(int64(h.Number*10 + uint64(i))), Token: Native, Sender: common.HexToAddress("0x5e"),
			Account: d.account, Amount: d.amount,
		})
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
	f.chain.mine(deposit{alice, eth(3), tx("a")})
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
	f.chain.mine(deposit{alice, eth(2), tx("a")})
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
	f.chain.mine(deposit{alice, eth(4), tx("a")})
	f.step(t)
	f.balance(t, alice, 4, 0)

	f.chain.reorg(1, 2, deposit{alice, eth(4), tx("a")}) // the same transaction in the new block 6
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
	f.chain.mine(deposit{bob, eth(1), tx("b")})
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
			f.chain.mine(deposit{alice, eth(1), tx("a1")}, deposit{bob, eth(2), tx("b1")})
		case 25:
			f.chain.mine(deposit{alice, eth(5), tx("a2")})
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
	f.chain.mine(deposit{alice, eth(3), tx("a")})
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
	f.chain.mine(deposit{alice, eth(1), tx("a")})
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
		f.chain.mine(deposit{account, eth(1), tx(fmt.Sprint(i))})
		want[account]++
		if i%10 == 9 {
			// The last deposit is dropped and comes back in the replacement block.
			f.chain.reorg(1, 1, deposit{account, eth(1), tx(fmt.Sprint(i))})
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
