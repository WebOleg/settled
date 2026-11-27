// Package ledger keeps the exchange's books in PostgreSQL: accounts and the transfers
// between them, double entry, in wei. The rules live in the database
// (migrations/0001_ledger.sql), where no caller can go around them; this package is
// the typed way in, and turns the database's refusals into errors to branch on.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Errors the database raises, by what went wrong.
var (
	// ErrConflict: the key, or the account name, was used before for something else.
	ErrConflict = errors.New("ledger: key used for another request")
	// ErrNotFound: the pending transfer or an account named does not exist.
	ErrNotFound = errors.New("ledger: not found")
	// ErrResolved: the pending transfer was already posted or voided.
	ErrResolved = errors.New("ledger: pending transfer already resolved")
	// ErrAppendOnly: something tried to change or delete a transfer.
	ErrAppendOnly = errors.New("ledger: transfers are append-only")
	// ErrAssetMismatch: the two accounts hold different assets.
	ErrAssetMismatch = errors.New("ledger: accounts hold different assets")
	// ErrExceedsCredits: the debit would take an account below zero.
	ErrExceedsCredits = errors.New("ledger: debits would exceed credits")
	// ErrExceedsDebits: the credit would take an account below zero the other way.
	ErrExceedsDebits = errors.New("ledger: credits would exceed debits")
)

// Kind is how a transfer moves money.
type Kind string

const (
	// Single posts at once.
	Single Kind = "single"
	// Pending reserves the amount: it is on hold on the debit side and not yet
	// spendable on the credit side.
	Pending Kind = "pending"
	// Post completes a pending transfer.
	Post Kind = "post"
	// Void cancels a pending transfer and releases the hold.
	Void Kind = "void"
)

// Limit is the balance rule of an account.
type Limit int

const (
	// NoLimit lets the account go either way.
	NoLimit Limit = iota
	// DebitsWithinCredits keeps a balance owed to someone at or above zero: a
	// customer's account.
	DebitsWithinCredits
	// CreditsWithinDebits keeps a balance held at or above zero: what the exchange
	// holds.
	CreditsWithinDebits
)

// Account is an account and its four totals.
type Account struct {
	ID             int64
	Name           string
	Asset          string
	Limit          Limit
	DebitsPending  *big.Int
	DebitsPosted   *big.Int
	CreditsPending *big.Int
	CreditsPosted  *big.Int
}

// Transfer is a request to move money, or a transfer recorded.
type Transfer struct {
	// Key is the caller's idempotency key: a request sent again with the same key and
	// the same fields returns the transfer already recorded.
	Key    string
	Kind   Kind
	Debit  int64
	Credit int64
	Amount *big.Int
	// PendingKey names the pending transfer a Post or Void resolves. Debit, Credit and
	// Amount may then be left zero: they are taken from it.
	PendingKey string
	Code       string

	// Set when recorded.
	ID        int64
	CreatedAt time.Time
}

// Ledger is a connection pool to the ledger database, as ledger_writer.
type Ledger struct {
	pool *pgxpool.Pool
}

// Open connects to the ledger database. url is a PostgreSQL connection string for a
// role with ledger_writer; it is never printed.
func Open(ctx context.Context, url string) (*Ledger, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("ledger: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ledger: connect: %w", err)
	}
	return &Ledger{pool: pool}, nil
}

// Close closes the pool.
func (l *Ledger) Close() { l.pool.Close() }

const accountColumns = "id, name, asset, debits_must_not_exceed_credits, credits_must_not_exceed_debits, " +
	"debits_pending::text, debits_posted::text, credits_pending::text, credits_posted::text"

// CreateAccount creates the account, or returns it if one of that name exists with the
// same asset and limit.
func (l *Ledger) CreateAccount(ctx context.Context, name, asset string, limit Limit) (Account, error) {
	row := l.pool.QueryRow(ctx, "SELECT "+accountColumns+" FROM ledger_account($1, $2, $3, $4)",
		name, asset, limit == DebitsWithinCredits, limit == CreditsWithinDebits)
	a, err := scanAccount(row)
	if err != nil {
		return Account{}, fmt.Errorf("ledger: account %s: %w", name, classify(err))
	}
	return a, nil
}

// Account reads an account by name.
func (l *Ledger) Account(ctx context.Context, name string) (Account, error) {
	a, err := scanAccount(l.pool.QueryRow(ctx, "SELECT "+accountColumns+" FROM accounts WHERE name = $1", name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("ledger: account %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Account{}, fmt.Errorf("ledger: account %s: %w", name, err)
	}
	return a, nil
}

// Transfer records t, or returns the transfer recorded before under its key.
func (l *Ledger) Transfer(ctx context.Context, t Transfer) (Transfer, error) {
	var debit, credit *int64
	var amount, pending *string
	if t.Debit != 0 {
		debit = &t.Debit
	}
	if t.Credit != 0 {
		credit = &t.Credit
	}
	if t.Amount != nil {
		if t.Amount.Sign() <= 0 {
			return Transfer{}, fmt.Errorf("ledger: transfer %s: amount %s is not positive", t.Key, t.Amount)
		}
		s := t.Amount.String()
		amount = &s
	}
	if t.PendingKey != "" {
		pending = &t.PendingKey
	}
	row := l.pool.QueryRow(ctx, `SELECT id, key, kind, debit_account, credit_account, amount::text,
		coalesce((SELECT key FROM transfers p WHERE p.id = t.pending_id), ''), code, created_at
		FROM ledger_transfer($1, $2, $3, $4, $5::numeric, $6, $7) t`,
		t.Key, string(t.Kind), debit, credit, amount, pending, t.Code)
	var out Transfer
	var kind, amt string
	if err := row.Scan(&out.ID, &out.Key, &kind, &out.Debit, &out.Credit, &amt, &out.PendingKey, &out.Code, &out.CreatedAt); err != nil {
		return Transfer{}, fmt.Errorf("ledger: transfer %s: %w", t.Key, classify(err))
	}
	out.Kind = Kind(kind)
	var ok bool
	if out.Amount, ok = new(big.Int).SetString(amt, 10); !ok {
		return Transfer{}, fmt.Errorf("ledger: transfer %s: amount %q", t.Key, amt)
	}
	return out, nil
}

// Check recomputes every balance from the journal and returns what disagrees, if
// anything; nil means the books are right.
func (l *Ledger) Check(ctx context.Context) ([]string, error) {
	rows, err := l.pool.Query(ctx, "SELECT problem FROM ledger_check()")
	if err != nil {
		return nil, fmt.Errorf("ledger: check: %w", err)
	}
	problems, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("ledger: check: %w", err)
	}
	return problems, nil
}

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var debitsLimited, creditsLimited bool
	var dp, dd, cp, cd string
	if err := row.Scan(&a.ID, &a.Name, &a.Asset, &debitsLimited, &creditsLimited, &dp, &dd, &cp, &cd); err != nil {
		return Account{}, err
	}
	switch {
	case debitsLimited:
		a.Limit = DebitsWithinCredits
	case creditsLimited:
		a.Limit = CreditsWithinDebits
	}
	for _, f := range []struct {
		dst **big.Int
		s   string
	}{{&a.DebitsPending, dp}, {&a.DebitsPosted, dd}, {&a.CreditsPending, cp}, {&a.CreditsPosted, cd}} {
		v, ok := new(big.Int).SetString(f.s, 10)
		if !ok {
			return Account{}, fmt.Errorf("balance %q", f.s)
		}
		*f.dst = v
	}
	return a, nil
}

// classify maps the database's refusals to the package's errors, keeping the original
// for its message.
func classify(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	var kind error
	switch pe.Code {
	case "LD001":
		kind = ErrConflict
	case "LD002":
		kind = ErrNotFound
	case "LD003":
		kind = ErrResolved
	case "LD004":
		kind = ErrAppendOnly
	case "LD005":
		kind = ErrAssetMismatch
	case "23514": // check_violation
		switch pe.ConstraintName {
		case "accounts_debits_within_credits":
			kind = ErrExceedsCredits
		case "accounts_credits_within_debits":
			kind = ErrExceedsDebits
		}
	}
	if kind == nil {
		return err
	}
	return fmt.Errorf("%w: %s", kind, pe.Message)
}
