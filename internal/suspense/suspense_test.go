package suspense

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/ledger/ledgertest"
)

var vault = common.HexToAddress("0x00000000000000000000000000000000000000aa")

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// fixture is a ledger with two operators, a and b, a reader who is not one, and the
// owner, who posts payments to suspense as the indexer would.
type fixture struct {
	owner  *ledger.Ledger
	a, b   *pgxpool.Pool
	reader *pgxpool.Pool
	events int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := ledgertest.New(t, ledger.Migrate, "suspense_operator")
	b := db.Login(t, "suspense_operator")
	reader := db.Login(t, "ledger_reader")
	f := &fixture{}
	var err error
	if f.owner, err = ledger.Open(ctx, db.OwnerURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.owner.Close)
	for _, c := range []struct {
		p   **pgxpool.Pool
		url string
	}{{&f.a, db.AppURL}, {&f.b, b}, {&f.reader, reader}} {
		if *c.p, err = pgxpool.New(ctx, c.url); err != nil {
			t.Fatal(err)
		}
		t.Cleanup((*c.p).Close)
	}
	return f
}

// paidWithoutTheLedger posts a final payment of n ETH to suspense, as the indexer does.
// The vault holds what it pays: depositor deposited it first.
func (f *fixture) paidWithoutTheLedger(t *testing.T, n int64, depositor string) int64 {
	t.Helper()
	ctx := context.Background()
	f.events++
	v, err := f.owner.CreateAccount(ctx, "vault:ETH", "ETH", ledger.CreditsWithinDebits)
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.owner.CreateAccount(ctx, deposits.SuspenseAccount("ETH"), "ETH", ledger.NoLimit)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.owner.CreateAccount(ctx, "customer:"+depositor+":ETH", "ETH", ledger.DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Transfer(ctx, ledger.Transfer{Key: fmt.Sprintf("deposit:%d", f.events), Kind: ledger.Single, Debit: v.ID, Credit: c.ID, Amount: eth(n), Code: "deposit"}); err != nil {
		t.Fatal(err)
	}
	bh := common.BytesToHash([]byte{byte(f.events)})
	var id int64
	if err := f.owner.Pool().QueryRow(ctx, `INSERT INTO withdrawal_events (vault, block_number, block_hash, log_index, tx_hash, withdrawal_id, kind,
		token, recipient, amount, status, outcome) VALUES ($1, 10, $2, 0, $2, $2, 'paid', $3, $4, $5::numeric, 'final', 'suspense') RETURNING id`,
		vault.Bytes(), bh.Bytes(), deposits.Native.Bytes(), common.HexToAddress("0xbeef").Bytes(), eth(n).String()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("payout:%x:%x:%d", vault.Bytes(), bh.Bytes(), 0)
	if _, err := f.owner.Transfer(ctx, ledger.Transfer{Key: key, Kind: ledger.Single, Debit: s.ID, Credit: v.ID, Amount: eth(n), Code: "payout-unmatched"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) balance(t *testing.T, name string) *big.Int {
	t.Helper()
	a, err := f.owner.Account(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return new(big.Int).Sub(a.DebitsPosted, a.CreditsPosted)
}

func (f *fixture) clean(t *testing.T) {
	t.Helper()
	problems, err := f.owner.Check(context.Background())
	if err != nil || len(problems) != 0 {
		t.Fatalf("the books disagree with the journal: %v %v", problems, err)
	}
}

func TestResolve_aSecondOperatorApprovesAWriteOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.paidWithoutTheLedger(t, 2, "depositor")
	open, err := List(ctx, f.a)
	if err != nil || len(open) != 1 || open[0].EventID != ev || open[0].Amount.Cmp(eth(2)) != 0 || open[0].ProposalID != nil {
		t.Fatalf("open: %+v, %v; want the 2 ETH payment, no proposal", open, err)
	}
	if n, age, err := Oldest(ctx, f.reader); err != nil || n != 1 || age < 0 {
		t.Fatalf("oldest: %d, %s, %v; want 1 waiting", n, age, err)
	}
	r, err := Propose(ctx, f.a, ev, WriteOff, "", "operator test payment, not a customer's")
	if err != nil || r.Status != "proposed" || r.Account != "writeoff:ETH" || r.Amount.Cmp(eth(2)) != 0 {
		t.Fatalf("propose: %+v, %v", r, err)
	}
	if open, _ := List(ctx, f.b); len(open) != 1 || open[0].ProposalID == nil || *open[0].ProposalID != r.ID {
		t.Fatalf("open after the proposal: %+v; want it waiting with the proposal", open)
	}
	if _, err := Approve(ctx, f.a, r.ID, "my own"); !errors.Is(err, ErrFourEyes) {
		t.Fatalf("the proposer approves: %v, want ErrFourEyes", err)
	}
	if f.balance(t, "suspense:ETH").Cmp(eth(2)) != 0 {
		t.Fatal("money moved before a second operator approved")
	}
	r, err = Approve(ctx, f.b, r.ID, "checked against the operator's log")
	if err != nil || r.Status != "approved" || r.DecidedBy == nil || *r.DecidedBy == r.ProposedBy {
		t.Fatalf("approve: %+v, %v", r, err)
	}
	if s, w := f.balance(t, "suspense:ETH"), f.balance(t, "writeoff:ETH"); s.Sign() != 0 || w.Cmp(eth(2)) != 0 {
		t.Fatalf("suspense %s, writeoff %s wei; want 0 and 2 ETH", s, w)
	}
	if open, _ := List(ctx, f.a); len(open) != 0 {
		t.Fatalf("open after approval: %+v, want none", open)
	}
	if n, age, err := Oldest(ctx, f.reader); err != nil || n != 0 || age != 0 {
		t.Fatalf("oldest after approval: %d, %s, %v; want none", n, age, err)
	}
	f.clean(t)
}

func TestResolve_aCustomerPaysOnlyWhatTheirBalanceCovers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.paidWithoutTheLedger(t, 3, "depositor") // the depositor has 3 ETH
	if _, err := Propose(ctx, f.a, ev, Customer, "customer:nobody:ETH", "paid by hand to a customer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown customer: %v, want ErrNotFound", err)
	}
	if _, err := Propose(ctx, f.a, ev, Customer, "vault:ETH", "paid by hand to a customer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an account that is not a customer's: %v, want ErrNotFound", err)
	}
	r, err := Propose(ctx, f.a, ev, Customer, "customer:depositor:ETH", "paid by hand to the depositor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(ctx, f.b, r.ID, "matches the ticket"); err != nil {
		t.Fatal(err)
	}
	if c := f.balance(t, "customer:depositor:ETH"); c.Sign() != 0 {
		t.Fatalf("the depositor holds %s wei more than nothing, want 0", new(big.Int).Neg(c))
	}
	// A second payment, of what another customer deposited: the depositor has nothing
	// left to pay it with.
	ev2 := f.paidWithoutTheLedger(t, 3, "other")
	r2, err := Propose(ctx, f.a, ev2, Customer, "customer:depositor:ETH", "paid by hand to the depositor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(ctx, f.b, r2.ID, "matches the ticket"); !errors.Is(err, ledger.ErrExceedsCredits) {
		t.Fatalf("approve beyond the balance: %v, want ledger.ErrExceedsCredits", err)
	}
	if open, _ := List(ctx, f.a); len(open) != 1 || open[0].ProposalID == nil || *open[0].ProposalID != r2.ID {
		t.Fatalf("after the refused approval: %+v, want the proposal still waiting", open)
	}
	f.clean(t)
}

func TestResolve_aPaymentIsResolvedOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.paidWithoutTheLedger(t, 1, "depositor")
	r, err := Propose(ctx, f.a, ev, WriteOff, "", "operator test payment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Propose(ctx, f.b, ev, WriteOff, "", "operator test payment"); !errors.Is(err, ErrProposed) {
		t.Fatalf("a second proposal: %v, want ErrProposed", err)
	}
	if _, err := Reject(ctx, f.b, r.ID, "no"); err == nil {
		t.Fatal("a rejection without a reason: accepted")
	}
	if _, err := Reject(ctx, f.b, r.ID, "the ticket names a customer"); err != nil {
		t.Fatal(err)
	}
	if _, err := Approve(ctx, f.a, r.ID, "too late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approving a rejected proposal: %v, want ErrNotFound", err)
	}
	r, err = Propose(ctx, f.a, ev, WriteOff, "", "operator test payment, confirmed")
	if err != nil {
		t.Fatalf("a new proposal after a rejection: %v", err)
	}
	if _, err := Approve(ctx, f.b, r.ID, "confirmed"); err != nil {
		t.Fatal(err)
	}
	if _, err := Propose(ctx, f.a, ev, WriteOff, "", "operator test payment, again"); !errors.Is(err, ErrProposed) {
		t.Fatalf("a proposal after approval: %v, want ErrProposed", err)
	}
	if f.balance(t, "writeoff:ETH").Cmp(eth(1)) != 0 {
		t.Fatal("the payment was written off more than once")
	}
	f.clean(t)
}

func TestResolve_onlyOperatorsAndOnlyThroughTheFunctions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ev := f.paidWithoutTheLedger(t, 1, "depositor")
	if _, err := Propose(ctx, f.reader, ev, WriteOff, "", "operator test payment"); !errors.Is(err, ErrNotOperator) {
		t.Fatalf("a reader proposes: %v, want ErrNotOperator", err)
	}
	r, err := Propose(ctx, f.a, ev, WriteOff, "", "operator test payment")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		fmt.Sprintf("UPDATE suspense_resolutions SET status = 'approved' WHERE id = %d", r.ID),
		fmt.Sprintf("INSERT INTO suspense_resolutions (event_id, kind, account, amount, reason, proposed_by, status) SELECT event_id, kind, account, amount, reason, 'someone', 'proposed' FROM suspense_resolutions WHERE id = %d", r.ID),
		"SELECT ledger_transfer('x', 'single', 1, 2, 1, NULL, 'x')",
	} {
		if _, err := f.b.Exec(ctx, q); err == nil {
			t.Errorf("an operator ran %q: accepted, want it refused", q)
		}
	}
	if _, err := Propose(ctx, f.a, 999, WriteOff, "", "no such payment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown payment: %v, want ErrNotFound", err)
	}
}
