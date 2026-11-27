// Package deposits credits the vault's deposits to the ledger, only once they are
// final. A deposit seen in a block not yet final is held in the ledger (a pending
// transfer); when the node's finalized block reaches it, it is posted; if its block
// leaves the chain first, it is voided. The indexer's view of the chain and the ledger
// change in one transaction, so a crash or a second indexer can neither lose a
// deposit nor count one twice. docs/design/deposits.md describes it.
package deposits

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/ledger"
)

// ErrFinalityViolation means the node's chain no longer contains a block the indexer
// held final: deposits were credited on a chain that is gone. The indexer writes
// nothing more until a person has looked.
var ErrFinalityViolation = errors.New("deposits: a final block left the chain")

// Indexer follows one vault.
type Indexer struct {
	Chain  Chain
	Ledger *ledger.Ledger
	Vault  common.Address
	// Start is the vault's deployment block: no deposit comes before it.
	Start uint64
	// MaxRange is the most final blocks read in one range; MaxBlocks the most blocks
	// not yet final read one by one in one step.
	MaxRange, MaxBlocks uint64
}

// Report is what one step did.
type Report struct {
	Head, Finalized uint64
	// Tip is the highest block the indexer has read.
	Tip uint64
	// Seen deposits are new, Posted ones became final, Voided ones left the chain.
	Seen, Posted, Voided int
	// Fork is the block a reorganisation went back to, when there was one.
	Fork *uint64
}

type stored struct {
	number uint64
	hash   common.Hash
	final  bool
}

// Step brings the ledger up to the chain once: it undoes what a reorganisation
// removed, reads new blocks, and posts what became final.
func (x *Indexer) Step(ctx context.Context) (Report, error) {
	var rep Report
	if x.Start == 0 {
		return rep, errors.New("deposits: the vault's deployment block is not set")
	}
	fin, err := x.Chain.Finalized(ctx)
	if err != nil {
		return rep, err
	}
	head, err := x.Chain.Latest(ctx)
	if err != nil {
		return rep, err
	}
	rep.Head, rep.Finalized = head.Number, fin.Number

	tip, ok, err := x.tip(ctx)
	if err != nil {
		return rep, err
	}
	if !ok {
		anchor, err := x.Chain.Header(ctx, x.Start-1)
		if err != nil {
			return rep, err
		}
		if err := x.anchor(ctx, anchor, anchor.Number <= fin.Number); err != nil {
			return rep, err
		}
		tip = stored{number: anchor.Number, hash: anchor.Hash}
	}

	// A reorganisation: the block at the tip, or at the head if the chain got shorter,
	// is not the one stored.
	n := min(tip.number, head.Number)
	h, err := x.Chain.Header(ctx, n)
	if err != nil {
		return rep, err
	}
	at, err := x.storedAt(ctx, n)
	if err != nil {
		return rep, err
	}
	if n < tip.number || at == nil || at.hash != h.Hash {
		fork, voided, err := x.unwind(ctx, tip, n)
		if err != nil {
			return rep, err
		}
		if fork == nil {
			return rep, nil // another indexer changed the tip; the next step looks again
		}
		rep.Fork, rep.Voided = fork, voided
		if tip, _, err = x.tip(ctx); err != nil {
			return rep, err
		}
	}

	// Final blocks, a range at a time.
	maxRange := max(x.MaxRange, 1)
	for tip.number < fin.Number {
		from, to := tip.number+1, min(fin.Number, tip.number+maxRange)
		last, err := x.Chain.Header(ctx, to)
		if err != nil {
			return rep, err
		}
		events, err := x.Chain.EventsInRange(ctx, x.Vault, from, to)
		if err != nil {
			return rep, err
		}
		seen, err := x.record(ctx, tip, last, events, true)
		if err != nil {
			return rep, err
		}
		if seen < 0 {
			return rep, nil // another indexer moved the tip; the next step goes on from it
		}
		rep.Seen += seen
		rep.Posted += seen
		tip = stored{number: last.Number, hash: last.Hash, final: true}
	}

	// Blocks not yet final, one at a time, each checked to sit on the one below.
	for i := uint64(0); tip.number < head.Number && i < max(x.MaxBlocks, 1); i++ {
		b, err := x.Chain.Header(ctx, tip.number+1)
		if err != nil {
			return rep, err
		}
		if b.Parent != tip.hash {
			break // the chain moved under us; the next step unwinds it
		}
		events, err := x.Chain.EventsInBlock(ctx, x.Vault, b.Hash)
		if err != nil {
			return rep, err
		}
		seen, err := x.record(ctx, tip, b, events, false)
		if err != nil {
			return rep, err
		}
		if seen < 0 {
			return rep, nil
		}
		rep.Seen += seen
		tip = stored{number: b.Number, hash: b.Hash}
	}
	rep.Tip = tip.number

	posted, err := x.finalize(ctx, fin)
	if err != nil {
		return rep, err
	}
	rep.Posted += posted
	return rep, nil
}

// lock serializes indexers of the same vault, transaction by transaction. Each
// transaction also checks the tip it builds on, so the lock only saves wasted work.
func (x *Indexer) lock(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('deposits:' || encode($1::bytea, 'hex'), 0))", x.Vault.Bytes())
	return err
}

func (x *Indexer) tip(ctx context.Context) (stored, bool, error) {
	var s stored
	var hash []byte
	err := x.Ledger.Pool().QueryRow(ctx, "SELECT number, hash, final FROM indexed_blocks WHERE vault = $1 ORDER BY number DESC LIMIT 1",
		x.Vault.Bytes()).Scan(&s.number, &hash, &s.final)
	if errors.Is(err, pgx.ErrNoRows) {
		return stored{}, false, nil
	}
	if err != nil {
		return stored{}, false, fmt.Errorf("deposits: read the tip: %w", err)
	}
	s.hash = common.BytesToHash(hash)
	return s, true, nil
}

func (x *Indexer) storedAt(ctx context.Context, n uint64) (*stored, error) {
	var hash []byte
	var final bool
	err := x.Ledger.Pool().QueryRow(ctx, "SELECT hash, final FROM indexed_blocks WHERE vault = $1 AND number = $2",
		x.Vault.Bytes(), n).Scan(&hash, &final)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("deposits: read block %d: %w", n, err)
	}
	return &stored{number: n, hash: common.BytesToHash(hash), final: final}, nil
}

func (x *Indexer) anchor(ctx context.Context, h Header, final bool) error {
	return x.Ledger.InTx(ctx, func(tx pgx.Tx, _ *ledger.Ledger) error {
		if err := x.lock(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO indexed_blocks (vault, number, hash, parent_hash, final) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT DO NOTHING`, x.Vault.Bytes(), h.Number, h.Hash.Bytes(), h.Parent.Bytes(), final)
		if err != nil {
			return fmt.Errorf("deposits: record the start block: %w", err)
		}
		return nil
	})
}

// unwind finds the highest stored block at or below n that is still in the chain, and
// in one transaction voids every pending deposit above it and forgets the blocks above
// it, if the tip is still the one the search started from. A final block is never
// unwound. A nil fork means the tip moved meanwhile and nothing was done.
func (x *Indexer) unwind(ctx context.Context, tip stored, n uint64) (*uint64, int, error) {
	rows, err := x.Ledger.Pool().Query(ctx, "SELECT number, hash, final FROM indexed_blocks WHERE vault = $1 AND number <= $2 ORDER BY number DESC",
		x.Vault.Bytes(), n)
	if err != nil {
		return nil, 0, fmt.Errorf("deposits: read stored blocks: %w", err)
	}
	blocks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stored, error) {
		var s stored
		var hash []byte
		err := r.Scan(&s.number, &hash, &s.final)
		s.hash = common.BytesToHash(hash)
		return s, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("deposits: read stored blocks: %w", err)
	}
	var fork *uint64
	for _, b := range blocks {
		h, err := x.Chain.Header(ctx, b.number)
		if err != nil {
			return nil, 0, err
		}
		if h.Hash == b.hash {
			fork = &b.number
			break
		}
		if b.final {
			return nil, 0, fmt.Errorf("%w: block %d was %s, the node now has %s", ErrFinalityViolation, b.number, b.hash, h.Hash)
		}
	}
	if fork == nil {
		return nil, 0, fmt.Errorf("%w: no stored block at or below %d is in the chain", ErrFinalityViolation, n)
	}
	voided, moved := 0, false
	err = x.Ledger.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
		if err := x.lock(ctx, tx); err != nil {
			return err
		}
		if moved, err = x.tipMoved(ctx, tx, tip); err != nil || moved {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE deposits SET status = 'voided' WHERE vault = $1 AND block_number > $2 AND status = 'pending'
			RETURNING ledger_key`, x.Vault.Bytes(), *fork)
		if err != nil {
			return fmt.Errorf("deposits: void: %w", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("deposits: void: %w", err)
		}
		for _, k := range keys {
			if _, err := in.Transfer(ctx, ledger.Transfer{Key: k + ":void", Kind: ledger.Void, PendingKey: k, Code: "deposit-reorg"}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM indexed_blocks WHERE vault = $1 AND number > $2", x.Vault.Bytes(), *fork); err != nil {
			return fmt.Errorf("deposits: forget blocks above %d: %w", *fork, err)
		}
		voided = len(keys)
		return nil
	})
	if err != nil || moved {
		return nil, 0, err
	}
	return fork, voided, nil
}

// tipMoved reports whether the stored tip is no longer tip, inside tx.
func (x *Indexer) tipMoved(ctx context.Context, tx pgx.Tx, tip stored) (bool, error) {
	var top uint64
	var hash []byte
	if err := tx.QueryRow(ctx, "SELECT number, hash FROM indexed_blocks WHERE vault = $1 ORDER BY number DESC LIMIT 1",
		x.Vault.Bytes()).Scan(&top, &hash); err != nil {
		return false, fmt.Errorf("deposits: read the tip: %w", err)
	}
	return top != tip.number || common.BytesToHash(hash) != tip.hash, nil
}

// record stores block b on top of tip with its deposits, held or, when final,
// credited at once. It returns -1 if the tip is no longer tip: another indexer got
// there first.
func (x *Indexer) record(ctx context.Context, tip stored, b Header, events []Event, final bool) (int, error) {
	seen := 0
	err := x.Ledger.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
		if err := x.lock(ctx, tx); err != nil {
			return err
		}
		moved, err := x.tipMoved(ctx, tx, tip)
		if err != nil {
			return err
		}
		if moved {
			seen = -1
			return nil
		}
		if _, err := tx.Exec(ctx, "INSERT INTO indexed_blocks (vault, number, hash, parent_hash, final) VALUES ($1, $2, $3, $4, $5)",
			x.Vault.Bytes(), b.Number, b.Hash.Bytes(), b.Parent.Bytes(), final); err != nil {
			return fmt.Errorf("deposits: record block %d: %w", b.Number, err)
		}
		for _, e := range events {
			if e.Block < tip.number+1 || e.Block > b.Number || (!final && e.BlockHash != b.Hash) {
				return fmt.Errorf("deposits: event %s/%d of block %d is outside %d..%d", e.BlockHash, e.LogIndex, e.Block, tip.number+1, b.Number)
			}
			if err := x.hold(ctx, tx, in, e, final); err != nil {
				return err
			}
			seen++
		}
		return nil
	})
	return seen, err
}

// hold records a deposit and holds it in the ledger, posting it at once when final.
func (x *Indexer) hold(ctx context.Context, tx pgx.Tx, in *ledger.Ledger, e Event, final bool) error {
	asset := Asset(e.Token)
	vault, err := in.CreateAccount(ctx, "vault:"+asset, asset, ledger.CreditsWithinDebits)
	if err != nil {
		return err
	}
	customer, err := in.CreateAccount(ctx, CustomerAccount(e.Account, asset), asset, ledger.DebitsWithinCredits)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("deposit:%x:%x:%d", x.Vault.Bytes(), e.BlockHash.Bytes(), e.LogIndex)
	if _, err := tx.Exec(ctx, `INSERT INTO deposits (vault, block_number, block_hash, log_index, tx_hash, deposit_id, token, sender, account, amount, status, ledger_key)
		VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9, $10::numeric, 'pending', $11)`,
		x.Vault.Bytes(), e.Block, e.BlockHash.Bytes(), e.LogIndex, e.TxHash.Bytes(), e.DepositID.String(),
		e.Token.Bytes(), e.Sender.Bytes(), e.Account.Bytes(), e.Amount.String(), key); err != nil {
		return fmt.Errorf("deposits: record deposit %s/%d: %w", e.BlockHash, e.LogIndex, err)
	}
	if _, err := in.Transfer(ctx, ledger.Transfer{Key: key, Kind: ledger.Pending, Debit: vault.ID, Credit: customer.ID, Amount: e.Amount, Code: "deposit"}); err != nil {
		return err
	}
	if final {
		return post(ctx, tx, in, key)
	}
	return nil
}

func post(ctx context.Context, tx pgx.Tx, in *ledger.Ledger, key string) error {
	if _, err := tx.Exec(ctx, "UPDATE deposits SET status = 'posted' WHERE ledger_key = $1", key); err != nil {
		return fmt.Errorf("deposits: post %s: %w", key, err)
	}
	_, err := in.Transfer(ctx, ledger.Transfer{Key: key + ":post", Kind: ledger.Post, PendingKey: key, Code: "deposit"})
	return err
}

// finalize posts every pending deposit at or below the node's finalized block, once the
// stored block at that height is the finalized one: the stored blocks above the last
// final one are a chain linked by parent hashes, so all of them below it are final too.
func (x *Indexer) finalize(ctx context.Context, fin Header) (int, error) {
	at, err := x.storedAt(ctx, fin.Number)
	if err != nil || at == nil || at.hash != fin.Hash {
		return 0, err // not read that far yet, or a reorganisation the next step unwinds
	}
	posted := 0
	err = x.Ledger.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
		if err := x.lock(ctx, tx); err != nil {
			return err
		}
		var hash []byte
		if err := tx.QueryRow(ctx, "SELECT hash FROM indexed_blocks WHERE vault = $1 AND number = $2", x.Vault.Bytes(), fin.Number).Scan(&hash); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("deposits: read block %d: %w", fin.Number, err)
		}
		if common.BytesToHash(hash) != fin.Hash {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE indexed_blocks SET final = true WHERE vault = $1 AND number <= $2 AND NOT final", x.Vault.Bytes(), fin.Number); err != nil {
			return fmt.Errorf("deposits: mark final: %w", err)
		}
		rows, err := tx.Query(ctx, "SELECT ledger_key FROM deposits WHERE vault = $1 AND block_number <= $2 AND status = 'pending' ORDER BY id",
			x.Vault.Bytes(), fin.Number)
		if err != nil {
			return fmt.Errorf("deposits: read pending: %w", err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("deposits: read pending: %w", err)
		}
		for _, k := range keys {
			if err := post(ctx, tx, in, k); err != nil {
				return err
			}
		}
		posted = len(keys)
		return nil
	})
	return posted, err
}

// Asset names what a token is in the ledger: ETH, or the token's address.
func Asset(token common.Address) string {
	if token == Native {
		return "ETH"
	}
	return strings.ToLower(token.Hex())
}

// CustomerAccount is the ledger account of an exchange account for an asset.
func CustomerAccount(account common.Hash, asset string) string {
	return "customer:" + account.Hex() + ":" + asset
}

// Pending returns the sum of the deposits held, not yet final, of an account.
func Pending(ctx context.Context, l *ledger.Ledger, account common.Hash, asset string) (*big.Int, error) {
	a, err := l.Account(ctx, CustomerAccount(account, asset))
	if errors.Is(err, ledger.ErrNotFound) {
		return new(big.Int), nil
	}
	if err != nil {
		return nil, err
	}
	return a.CreditsPending, nil
}
