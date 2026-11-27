package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// adminEnv names the connection string of a PostgreSQL superuser for the tests. make
// check-go starts a throwaway PostgreSQL and sets it; each test gets a database of its
// own. Without it the tests fail rather than skip: a ledger left untested reads as a
// ledger that passed.
const adminEnv = "SETTLED_TEST_POSTGRES"

const appRole = "ledger_test_app"

type fixture struct {
	admin *pgx.Conn // the schema owner, in the test's database
	l     *Ledger   // the service, as a ledger_writer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	adminURL := os.Getenv(adminEnv)
	if adminURL == "" {
		t.Fatalf("%s is not set; run make check-go", adminEnv)
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer server.Close(ctx)
	db := "ledger_test_" + randomHex(t, 6)
	if _, err := server.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	password := randomHex(t, 16)
	// Roles belong to the server, not to one database: created once, password reset.
	if _, err := server.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s LOGIN; END IF;
		END $$`, appRole, appRole)); err != nil {
		t.Fatalf("create the app role: %v", err)
	}
	if _, err := server.Exec(ctx, fmt.Sprintf("ALTER ROLE %s PASSWORD '%s'", appRole, password)); err != nil {
		t.Fatalf("set the app role's password: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
	})

	dbURL := withDatabase(t, adminURL, db, "", "")
	if n, err := Migrate(ctx, dbURL); err != nil || n != 1 {
		t.Fatalf("migrate: %d applied, %v", n, err)
	}
	admin, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	if _, err := admin.Exec(ctx, "GRANT ledger_writer TO "+appRole); err != nil {
		t.Fatalf("grant ledger_writer: %v", err)
	}
	l, err := Open(ctx, withDatabase(t, adminURL, db, appRole, password))
	if err != nil {
		t.Fatalf("open as the app role: %v", err)
	}
	t.Cleanup(l.Close)
	return &fixture{admin: admin, l: l}
}

func withDatabase(t *testing.T, raw, db, user, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", adminEnv, err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// books makes the exchange's ETH (an asset) and two customers (liabilities).
func (f *fixture) books(t *testing.T) (vault, alice, bob Account) {
	t.Helper()
	ctx := context.Background()
	var err error
	if vault, err = f.l.CreateAccount(ctx, "vault:ETH", "ETH", CreditsWithinDebits); err != nil {
		t.Fatal(err)
	}
	if alice, err = f.l.CreateAccount(ctx, "customer:alice:ETH", "ETH", DebitsWithinCredits); err != nil {
		t.Fatal(err)
	}
	if bob, err = f.l.CreateAccount(ctx, "customer:bob:ETH", "ETH", DebitsWithinCredits); err != nil {
		t.Fatal(err)
	}
	return vault, alice, bob
}

func (f *fixture) must(t *testing.T, tr Transfer) Transfer {
	t.Helper()
	out, err := f.l.Transfer(context.Background(), tr)
	if err != nil {
		t.Fatalf("transfer %s: %v", tr.Key, err)
	}
	return out
}

func (f *fixture) balances(t *testing.T, name string, want ...int64) {
	t.Helper()
	a, err := f.l.Account(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	got := []*big.Int{a.DebitsPending, a.DebitsPosted, a.CreditsPending, a.CreditsPosted}
	for i, w := range want {
		if got[i].Cmp(eth(w)) != 0 {
			t.Fatalf("%s: debits pending/posted, credits pending/posted = %v, want %v ETH", name, got, want)
		}
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

func TestMigrate_appliesOnceAndRefusesAChangedMigration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	url := f.admin.Config().ConnString()
	if n, err := Migrate(ctx, url); err != nil || n != 0 {
		t.Fatalf("second migrate: %d applied, %v; want 0, nil", n, err)
	}
	if _, err := f.admin.Exec(ctx, "UPDATE schema_migrations SET sha256 = 'changed' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, url); !errors.Is(err, ErrMigrationChanged) {
		t.Fatalf("migrate over a changed migration: %v, want ErrMigrationChanged", err)
	}
}

func TestAccount_sameNameSameSettingsIsTheSameAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := f.l.CreateAccount(ctx, "customer:alice:ETH", "ETH", DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.l.CreateAccount(ctx, "customer:alice:ETH", "ETH", DebitsWithinCredits)
	if err != nil || b.ID != a.ID {
		t.Fatalf("again: %+v, %v; want account %d", b, err, a.ID)
	}
	if _, err := f.l.CreateAccount(ctx, "customer:alice:ETH", "ETH", NoLimit); !errors.Is(err, ErrConflict) {
		t.Fatalf("other settings: %v, want ErrConflict", err)
	}
	if _, err := f.l.Account(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v, want ErrNotFound", err)
	}
}

func TestTransfer_depositIsHeldUntilPosted(t *testing.T) {
	f := newFixture(t)
	vault, alice, bob := f.books(t)
	f.must(t, Transfer{Key: "deposit-1", Kind: Pending, Debit: vault.ID, Credit: alice.ID, Amount: eth(5), Code: "deposit"})
	f.balances(t, alice.Name, 0, 0, 5, 0)
	f.balances(t, vault.Name, 5, 0, 0, 0)

	// A pending credit is not there to spend.
	_, err := f.l.Transfer(context.Background(), Transfer{Key: "spend", Kind: Single, Debit: alice.ID, Credit: bob.ID, Amount: eth(1), Code: "trade"})
	if !errors.Is(err, ErrExceedsCredits) {
		t.Fatalf("spending a pending deposit: %v, want ErrExceedsCredits", err)
	}

	post := f.must(t, Transfer{Key: "deposit-1:post", Kind: Post, PendingKey: "deposit-1", Code: "deposit"})
	if post.Debit != vault.ID || post.Credit != alice.ID || post.Amount.Cmp(eth(5)) != 0 || post.PendingKey != "deposit-1" {
		t.Fatalf("post took %+v, want the pending transfer's accounts and amount", post)
	}
	f.balances(t, alice.Name, 0, 0, 0, 5)
	f.balances(t, vault.Name, 0, 5, 0, 0)
	f.must(t, Transfer{Key: "spend", Kind: Single, Debit: alice.ID, Credit: bob.ID, Amount: eth(1), Code: "trade"})
	f.balances(t, alice.Name, 0, 1, 0, 5)
	f.clean(t)
}

func TestTransfer_pendingIsResolvedOnce(t *testing.T) {
	f := newFixture(t)
	vault, alice, _ := f.books(t)
	ctx := context.Background()
	f.must(t, Transfer{Key: "deposit-1", Kind: Pending, Debit: vault.ID, Credit: alice.ID, Amount: eth(2), Code: "deposit"})
	f.must(t, Transfer{Key: "deposit-1:void", Kind: Void, PendingKey: "deposit-1", Code: "deposit"})
	f.balances(t, alice.Name, 0, 0, 0, 0)
	for _, k := range []Kind{Post, Void} {
		_, err := f.l.Transfer(ctx, Transfer{Key: "deposit-1:" + string(k) + ":again", Kind: k, PendingKey: "deposit-1", Code: "deposit"})
		if !errors.Is(err, ErrResolved) {
			t.Fatalf("%s after void: %v, want ErrResolved", k, err)
		}
	}
	if _, err := f.l.Transfer(ctx, Transfer{Key: "x", Kind: Post, PendingKey: "no-such", Code: "deposit"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post of an unknown key: %v, want ErrNotFound", err)
	}
	if _, err := f.l.Transfer(ctx, Transfer{Key: "y", Kind: Post, PendingKey: "deposit-1:void", Code: "deposit"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post of a void: %v, want ErrNotFound", err)
	}
	if _, err := f.l.Transfer(ctx, Transfer{Key: "z", Kind: Post, PendingKey: "deposit-1", Amount: eth(1), Code: "deposit"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("post of another amount: %v, want ErrConflict", err)
	}
	f.clean(t)
}

func TestTransfer_sameKeyIsOneTransfer(t *testing.T) {
	f := newFixture(t)
	vault, alice, bob := f.books(t)
	ctx := context.Background()
	req := Transfer{Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(3), Code: "deposit"}
	first := f.must(t, req)
	again := f.must(t, req)
	if again.ID != first.ID {
		t.Fatalf("the same request again made transfer %d, want %d", again.ID, first.ID)
	}
	f.balances(t, alice.Name, 0, 0, 0, 3)
	for name, other := range map[string]Transfer{
		"amount": {Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(4), Code: "deposit"},
		"credit": {Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: bob.ID, Amount: eth(3), Code: "deposit"},
		"kind":   {Key: "deposit-1", Kind: Pending, Debit: vault.ID, Credit: alice.ID, Amount: eth(3), Code: "deposit"},
		"code":   {Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(3), Code: "trade"},
	} {
		if _, err := f.l.Transfer(ctx, other); !errors.Is(err, ErrConflict) {
			t.Fatalf("same key, other %s: %v, want ErrConflict", name, err)
		}
	}
	f.balances(t, alice.Name, 0, 0, 0, 3)
	f.clean(t)
}

func TestTransfer_holdsCountAgainstTheBalance(t *testing.T) {
	f := newFixture(t)
	vault, alice, _ := f.books(t)
	ctx := context.Background()
	f.must(t, Transfer{Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(5), Code: "deposit"})
	f.must(t, Transfer{Key: "withdrawal-1", Kind: Pending, Debit: alice.ID, Credit: vault.ID, Amount: eth(4), Code: "withdrawal"})
	_, err := f.l.Transfer(ctx, Transfer{Key: "withdrawal-2", Kind: Pending, Debit: alice.ID, Credit: vault.ID, Amount: eth(2), Code: "withdrawal"})
	if !errors.Is(err, ErrExceedsCredits) {
		t.Fatalf("a second hold beyond the balance: %v, want ErrExceedsCredits", err)
	}
	// The vault cannot pay out what it never took in.
	_, err = f.l.Transfer(ctx, Transfer{Key: "out", Kind: Single, Debit: alice.ID, Credit: vault.ID, Amount: eth(1), Code: "withdrawal"})
	if err != nil {
		t.Fatal(err)
	}
	carol, err := f.l.CreateAccount(ctx, "customer:carol:ETH", "ETH", NoLimit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.l.Transfer(ctx, Transfer{Key: "too-much", Kind: Single, Debit: carol.ID, Credit: vault.ID, Amount: eth(1), Code: "withdrawal"})
	if !errors.Is(err, ErrExceedsDebits) {
		t.Fatalf("crediting the vault beyond what it holds: %v, want ErrExceedsDebits", err)
	}
	if _, err := f.l.Transfer(ctx, Transfer{Key: "zero", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: new(big.Int), Code: "deposit"}); err == nil {
		t.Fatal("a zero transfer was recorded")
	}
	f.clean(t)
}

func TestTransfer_oneAssetPerTransfer(t *testing.T) {
	f := newFixture(t)
	vault, _, _ := f.books(t)
	ctx := context.Background()
	usdc, err := f.l.CreateAccount(ctx, "customer:alice:USDC", "USDC", DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.l.Transfer(ctx, Transfer{Key: "mix", Kind: Single, Debit: vault.ID, Credit: usdc.ID, Amount: eth(1), Code: "deposit"})
	if !errors.Is(err, ErrAssetMismatch) {
		t.Fatalf("ETH into a USDC account: %v, want ErrAssetMismatch", err)
	}
	_, err = f.l.Transfer(ctx, Transfer{Key: "self", Kind: Single, Debit: vault.ID, Credit: vault.ID, Amount: eth(1), Code: "deposit"})
	if err == nil {
		t.Fatal("a transfer from an account to itself was recorded")
	}
}

func TestLedger_theServiceCannotWriteAroundTheRules(t *testing.T) {
	f := newFixture(t)
	vault, alice, _ := f.books(t)
	ctx := context.Background()
	f.must(t, Transfer{Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(1), Code: "deposit"})
	for _, q := range []string{
		"UPDATE accounts SET credits_posted = 1000",
		"INSERT INTO transfers (key, kind, debit_account, credit_account, amount, code) VALUES ('x', 'single', 1, 2, 1, 'x')",
		"DELETE FROM transfers",
		"TRUNCATE transfers",
	} {
		_, err := f.l.pool.Exec(ctx, q)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("%s as the service: %v, want permission denied", q, err)
		}
	}
	// Even the owner cannot change the journal.
	for _, q := range []string{"UPDATE transfers SET amount = 2", "DELETE FROM transfers", "TRUNCATE transfers"} {
		if _, err := f.admin.Exec(ctx, q); !errors.Is(classify(err), ErrAppendOnly) {
			t.Fatalf("%s as the owner: %v, want ErrAppendOnly", q, err)
		}
	}
	f.clean(t)
}

func TestCheck_findsABalanceChangedByHand(t *testing.T) {
	f := newFixture(t)
	vault, alice, _ := f.books(t)
	ctx := context.Background()
	f.must(t, Transfer{Key: "deposit-1", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(1), Code: "deposit"})
	f.clean(t)
	if _, err := f.admin.Exec(ctx, "UPDATE accounts SET credits_posted = credits_posted + 1 WHERE id = $1", alice.ID); err != nil {
		t.Fatal(err)
	}
	problems, err := f.l.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], alice.Name) {
		t.Fatalf("check after a hand edit: %v, want the account and the totals", problems)
	}
}

// Many workers move money between the same accounts at once, each request sent twice:
// every request is recorded once, no balance goes below zero, and the books agree.
func TestTransfer_concurrentRequestsKeepTheBooks(t *testing.T) {
	f := newFixture(t)
	vault, alice, bob := f.books(t)
	ctx := context.Background()
	f.must(t, Transfer{Key: "seed-alice", Kind: Single, Debit: vault.ID, Credit: alice.ID, Amount: eth(10), Code: "deposit"})
	f.must(t, Transfer{Key: "seed-bob", Kind: Single, Debit: vault.ID, Credit: bob.ID, Amount: eth(10), Code: "deposit"})

	const workers, each = 16, 25
	var mu sync.Mutex
	succeeded := map[string]int{}
	var wg sync.WaitGroup
	errs := make(chan error, workers*each*2)
	for w := range workers {
		for i := range each {
			from, to := alice.ID, bob.ID
			if (w+i)%2 == 1 {
				from, to = bob.ID, alice.ID
			}
			req := Transfer{Key: fmt.Sprintf("move-%d-%d", w, i), Kind: Single, Debit: from, Credit: to, Amount: eth(1), Code: "trade"}
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// A refused request leaves no trace, so its retry may still go
					// through once the balance allows it.
					_, err := f.l.Transfer(ctx, req)
					switch {
					case err == nil:
						mu.Lock()
						succeeded[req.Key]++
						mu.Unlock()
					case !errors.Is(err, ErrExceedsCredits):
						errs <- err
					}
				}()
			}
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int64
	if err := f.admin.QueryRow(ctx, "SELECT count(*) FROM transfers WHERE code = 'trade'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != int64(len(succeeded)) || n == 0 {
		t.Fatalf("%d trades in the journal for %d keys that succeeded, want one each", n, len(succeeded))
	}
	a, _ := f.l.Account(ctx, alice.Name)
	b, _ := f.l.Account(ctx, bob.Name)
	total := new(big.Int).Add(new(big.Int).Sub(a.CreditsPosted, a.DebitsPosted), new(big.Int).Sub(b.CreditsPosted, b.DebitsPosted))
	if total.Cmp(eth(20)) != 0 {
		t.Fatalf("alice and bob hold %s wei together, want 20 ETH", total)
	}
	f.clean(t)
}

// A post and a void of the same pending transfer, at once: exactly one wins.
func TestTransfer_postAndVoidRaceOneWins(t *testing.T) {
	f := newFixture(t)
	vault, alice, _ := f.books(t)
	ctx := context.Background()
	for round := range 20 {
		key := fmt.Sprintf("deposit-%d", round)
		f.must(t, Transfer{Key: key, Kind: Pending, Debit: vault.ID, Credit: alice.ID, Amount: eth(1), Code: "deposit"})
		var wg sync.WaitGroup
		results := make([]error, 4)
		for i := range results {
			k := Post
			if i%2 == 1 {
				k = Void
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = f.l.Transfer(ctx, Transfer{Key: fmt.Sprintf("%s:%s:%d", key, k, i), Kind: k, PendingKey: key, Code: "deposit"})
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range results {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, ErrResolved):
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d resolutions won, want 1", round, won)
		}
	}
	f.clean(t)
}
