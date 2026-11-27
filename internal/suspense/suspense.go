// Package suspense resolves payments in suspense under four eyes: money that left the
// vault without the ledger, posted to the suspense account by the deposit indexer. One
// operator proposes how a payment is explained, a second approves it, and only the
// approval moves the money. The rules live in the database (migration 0008): each
// operator connects as a login of their own, the database tells them apart by it, and
// no one can approve their own proposal. This package is the client of those rules.
package suspense

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/WebOleg/settled/internal/ledger"
)

var (
	// ErrNotOperator: the login does not hold suspense_operator.
	ErrNotOperator = errors.New("suspense: not a suspense operator")
	// ErrFourEyes: the operator who proposed tried to approve.
	ErrFourEyes = errors.New("suspense: four eyes")
	// ErrProposed: the payment already has a proposal open or approved.
	ErrProposed = errors.New("suspense: already proposed")
	// ErrNotFound: no such payment in suspense, or no such open proposal or account.
	ErrNotFound = errors.New("suspense: not found")
)

// Kind is how a payment is explained.
type Kind string

const (
	// WriteOff: the money is lost to the exchange, posted to writeoff:<asset>.
	WriteOff Kind = "writeoff"
	// Customer: it was owed to a customer and paid outside the ledger; their balance
	// pays it, and only if it covers it.
	Customer Kind = "customer"
)

// DB is what the package reads and writes through: a pool, a connection or a
// transaction, connected as the operator.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Open is a payment in suspense not yet explained.
type Open struct {
	EventID      int64
	Block        uint64
	Tx           common.Hash
	WithdrawalID common.Hash
	Recipient    common.Address
	Asset        string
	Amount       *big.Int
	PostedAt     time.Time
	// The proposal waiting for a second operator, if any.
	ProposalID *int64
	ProposedBy *string
	Proposal   *Kind
}

// Resolution is a proposal and, once made, its decision.
type Resolution struct {
	ID         int64
	EventID    int64
	Kind       Kind
	Account    string
	Amount     *big.Int
	Reason     string
	ProposedBy string
	Status     string
	DecidedBy  *string
	Note       *string
}

// List returns the payments in suspense not yet explained, oldest first.
func List(ctx context.Context, db DB) ([]Open, error) {
	rows, err := db.Query(ctx, `SELECT event_id, block_number, tx_hash, withdrawal_id, recipient, asset, amount::text, posted_at,
		proposal_id, proposed_by, proposal_kind FROM suspense_open ORDER BY posted_at, event_id`)
	if err != nil {
		return nil, classify(err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Open, error) {
		var o Open
		var tx, wid, recipient []byte
		var amount string
		var by *string
		var kind *string
		err := r.Scan(&o.EventID, &o.Block, &tx, &wid, &recipient, &o.Asset, &amount, &o.PostedAt, &o.ProposalID, &by, &kind)
		o.Tx, o.WithdrawalID, o.Recipient = common.BytesToHash(tx), common.BytesToHash(wid), common.BytesToAddress(recipient)
		o.Amount, _ = new(big.Int).SetString(amount, 10)
		o.ProposedBy = by
		if kind != nil {
			k := Kind(*kind)
			o.Proposal = &k
		}
		return o, err
	})
}

// Oldest returns how many payments wait in suspense and how long the oldest has, by
// the database's clock; zero when none does.
func Oldest(ctx context.Context, db DB) (int64, time.Duration, error) {
	var n int64
	var age float64
	err := db.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(posted_at)), 0)::float8 FROM suspense_open`).Scan(&n, &age)
	if err != nil {
		return 0, 0, classify(err)
	}
	return n, time.Duration(age * float64(time.Second)), nil
}

// Propose records how the payment of event is to be explained: written off, or charged
// to customer (an account name), with a reason of ten characters or more.
func Propose(ctx context.Context, db DB, event int64, kind Kind, customer, reason string) (Resolution, error) {
	var c *string
	if kind == Customer {
		c = &customer
	}
	return one(ctx, db, "suspense_propose($1, $2, $3, $4)", event, string(kind), c, reason)
}

// Approve moves the money of proposal id as proposed. The operator who proposed it
// cannot.
func Approve(ctx context.Context, db DB, id int64, note string) (Resolution, error) {
	return one(ctx, db, "suspense_approve($1, $2)", id, note)
}

// Reject closes proposal id without moving anything, saying why in ten characters or
// more; the payment may be proposed again.
func Reject(ctx context.Context, db DB, id int64, note string) (Resolution, error) {
	return one(ctx, db, "suspense_reject($1, $2)", id, note)
}

func one(ctx context.Context, db DB, call string, args ...any) (Resolution, error) {
	var r Resolution
	var amount, kind string
	var account int64
	err := db.QueryRow(ctx, `SELECT id, event_id, kind, account, amount::text, reason, proposed_by, status, decided_by, decision_note FROM `+call, args...).
		Scan(&r.ID, &r.EventID, &kind, &account, &amount, &r.Reason, &r.ProposedBy, &r.Status, &r.DecidedBy, &r.Note)
	if err != nil {
		return Resolution{}, classify(err)
	}
	r.Kind = Kind(kind)
	r.Amount, _ = new(big.Int).SetString(amount, 10)
	// A statement sees the rows as they were when it began, not an account the call made:
	// the name is read after it.
	if err := db.QueryRow(ctx, "SELECT name FROM accounts WHERE id = $1", account).Scan(&r.Account); err != nil {
		return Resolution{}, fmt.Errorf("suspense: read account %d: %w", account, err)
	}
	return r, nil
}

func classify(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	var kind error
	switch pe.Code {
	case "42501":
		kind = ErrNotOperator
	case "LD008":
		kind = ErrFourEyes
	case "LD003":
		kind = ErrProposed
	case "LD002":
		kind = ErrNotFound
	}
	if kind == nil {
		return ledger.Classify(err)
	}
	return fmt.Errorf("%w: %s", kind, strings.TrimPrefix(pe.Message, "suspense: "))
}
