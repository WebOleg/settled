// Package deposits keeps the ledger in step with the vault's money events, only once
// they are final. A deposit seen in a block not yet final is held in the ledger (a
// pending transfer); when the node's finalized block reaches it, it is posted; if its
// block leaves the chain first, it is voided. A final payment settles its withdrawal
// (internal/withdrawals) and a final cancellation releases it; a payment the ledger
// never requested is posted to the suspense account. The indexer's view of the chain
// and the ledger change in one transaction, so a crash or a second indexer can
// neither lose an event nor count one twice. docs/design/deposits.md describes it.
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
	// only, in tests, reads fewer streams, to stand for an older version.
	only []string
}

// The streams of events the indexer reads, each from its own starting block
// (indexer_streams).
const (
	StreamDeposits    = "deposits"
	StreamWithdrawals = "withdrawals"
)

var allStreams = []string{StreamDeposits, StreamWithdrawals}

func streamOf(k Kind) string {
	if k == KindDeposit {
		return StreamDeposits
	}
	return StreamWithdrawals
}

// position is where a stream is read from: live from liveFrom on, and below it the
// final blocks up to backfilledTo.
type position struct {
	liveFrom, backfilledTo uint64
}

func (x *Indexer) streams() []string {
	if len(x.only) == 0 {
		return allStreams
	}
	return x.only
}

// Report is what one step did.
type Report struct {
	Head, Finalized uint64
	// Tip is the highest block the indexer has read.
	Tip uint64
	// Seen deposits are new, Posted ones became final, Voided ones left the chain.
	Seen, Posted, Voided int
	// Backfilled final blocks read again for a stream added later; Backlog of them
	// still to read.
	Backfilled, Backlog uint64
	// Final payments and cancellations: Settled and Cancelled withdrawals of the
	// ledger, and Suspense payments it never requested.
	Settled, Cancelled, Suspense int
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
	pos, err := x.positions(ctx)
	if err != nil {
		return rep, err
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

	// Before final blocks are read a range at a time, what is held at or below the tip
	// is final too, since the tip is at or below the finalized block and was just found
	// in the chain: it is applied first, so events apply in chain order.
	if tip.number < fin.Number {
		posted, err := x.finalize(ctx, Header{Number: tip.number, Hash: tip.hash}, &rep)
		if err != nil {
			return rep, err
		}
		rep.Posted += posted
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
		seen, err := x.record(ctx, tip, last, live(events, pos), true, &rep)
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
		seen, err := x.record(ctx, tip, b, live(events, pos), false, &rep)
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

	posted, err := x.finalize(ctx, fin, &rep)
	if err != nil {
		return rep, err
	}
	rep.Posted += posted
	if err := x.backfill(ctx, pos, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// live keeps the events of the streams read live at their block.
func live(events []Event, pos map[string]position) []Event {
	out := events[:0:0]
	for _, e := range events {
		if p, ok := pos[streamOf(e.Kind)]; ok && e.Block >= p.liveFrom {
			out = append(out, e)
		}
	}
	return out
}

// positions reads where each stream is read from. A stream with no position yet is
// read from the vault's deployment block, if the indexer has read nothing yet; a stream
// added after the indexer read blocks gets its position from its migration, which knows
// the tip it was added at.
func (x *Indexer) positions(ctx context.Context) (map[string]position, error) {
	pos := map[string]position{}
	err := x.Ledger.InTx(ctx, func(tx pgx.Tx, _ *ledger.Ledger) error {
		if err := x.lock(ctx, tx); err != nil {
			return err
		}
		var read int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM indexed_blocks WHERE vault = $1 AND number >= $2", x.Vault.Bytes(), x.Start).Scan(&read); err != nil {
			return fmt.Errorf("deposits: read the stored blocks: %w", err)
		}
		for _, st := range x.streams() {
			var p position
			err := tx.QueryRow(ctx, "SELECT live_from, backfilled_to FROM indexer_streams WHERE vault = $1 AND stream = $2", x.Vault.Bytes(), st).
				Scan(&p.liveFrom, &p.backfilledTo)
			if errors.Is(err, pgx.ErrNoRows) {
				if read > 0 {
					return fmt.Errorf("deposits: stream %s has no starting block, and blocks were read without it; its migration sets one", st)
				}
				p = position{liveFrom: x.Start, backfilledTo: x.Start - 1}
				if _, err := tx.Exec(ctx, "INSERT INTO indexer_streams (vault, stream, live_from, backfilled_to) VALUES ($1, $2, $3, $4)",
					x.Vault.Bytes(), st, p.liveFrom, p.backfilledTo); err != nil {
					return fmt.Errorf("deposits: start stream %s: %w", st, err)
				}
			} else if err != nil {
				return fmt.Errorf("deposits: read stream %s: %w", st, err)
			}
			pos[st] = p
		}
		return nil
	})
	return pos, err
}

// backfill reads, for each stream added after blocks were read, the final blocks it
// missed, a range at a time, up to the block before it went live. The events are
// final, so they apply at once. Blocks not yet final wait until they are.
func (x *Indexer) backfill(ctx context.Context, pos map[string]position, rep *Report) error {
	var final uint64
	if err := x.Ledger.Pool().QueryRow(ctx, "SELECT coalesce(max(number) FILTER (WHERE final), 0) FROM indexed_blocks WHERE vault = $1",
		x.Vault.Bytes()).Scan(&final); err != nil {
		return fmt.Errorf("deposits: read the last final block: %w", err)
	}
	maxRange := max(x.MaxRange, 1)
	for _, st := range x.streams() {
		p := pos[st]
		for range 8 { // a bounded share of a step
			if p.backfilledTo+1 >= p.liveFrom {
				break
			}
			from, to := p.backfilledTo+1, min(p.liveFrom-1, final, p.backfilledTo+maxRange)
			if to < from {
				break
			}
			events, err := x.Chain.EventsInRange(ctx, x.Vault, from, to)
			if err != nil {
				return err
			}
			moved := false
			err = x.Ledger.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
				if err := x.lock(ctx, tx); err != nil {
					return err
				}
				var at uint64
				if err := tx.QueryRow(ctx, "SELECT backfilled_to FROM indexer_streams WHERE vault = $1 AND stream = $2 FOR UPDATE",
					x.Vault.Bytes(), st).Scan(&at); err != nil {
					return fmt.Errorf("deposits: read stream %s: %w", st, err)
				}
				if at != p.backfilledTo {
					moved = true // another indexer did this range
					return nil
				}
				for _, e := range events {
					if streamOf(e.Kind) != st || e.Block < from || e.Block > to {
						continue
					}
					var err error
					if e.Kind == KindDeposit {
						err = x.hold(ctx, tx, in, e, true)
					} else {
						err = x.noteWithdrawal(ctx, tx, in, e, true, rep)
					}
					if err != nil {
						return err
					}
				}
				_, err := tx.Exec(ctx, "UPDATE indexer_streams SET backfilled_to = $3 WHERE vault = $1 AND stream = $2", x.Vault.Bytes(), st, to)
				return err
			})
			if err != nil {
				return fmt.Errorf("deposits: backfill %s %d..%d: %w", st, from, to, err)
			}
			if moved {
				break
			}
			rep.Backfilled += to - from + 1
			p.backfilledTo = to
		}
		if p.backfilledTo+1 < p.liveFrom {
			rep.Backlog += p.liveFrom - 1 - p.backfilledTo
		}
	}
	return nil
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
		_, err := tx.Exec(ctx, `INSERT INTO indexed_blocks (vault, number, hash, parent_hash, final, streams) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING`, x.Vault.Bytes(), h.Number, h.Hash.Bytes(), h.Parent.Bytes(), final, x.streams())
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
		// Payments and cancellations not yet final did nothing to the ledger yet.
		if _, err := tx.Exec(ctx, "UPDATE withdrawal_events SET status = 'voided' WHERE vault = $1 AND block_number > $2 AND status = 'pending'",
			x.Vault.Bytes(), *fork); err != nil {
			return fmt.Errorf("deposits: void withdrawal events: %w", err)
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
func (x *Indexer) record(ctx context.Context, tip stored, b Header, events []Event, final bool, rep *Report) (int, error) {
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
		if _, err := tx.Exec(ctx, "INSERT INTO indexed_blocks (vault, number, hash, parent_hash, final, streams) VALUES ($1, $2, $3, $4, $5, $6)",
			x.Vault.Bytes(), b.Number, b.Hash.Bytes(), b.Parent.Bytes(), final, x.streams()); err != nil {
			return fmt.Errorf("deposits: record block %d: %w", b.Number, err)
		}
		for _, e := range events {
			if e.Block < tip.number+1 || e.Block > b.Number || (!final && e.BlockHash != b.Hash) {
				return fmt.Errorf("deposits: event %s/%d of block %d is outside %d..%d", e.BlockHash, e.LogIndex, e.Block, tip.number+1, b.Number)
			}
			switch e.Kind {
			case KindDeposit:
				if err := x.hold(ctx, tx, in, e, final); err != nil {
					return err
				}
				seen++
			case KindPaid, KindCancelled:
				if err := x.noteWithdrawal(ctx, tx, in, e, final, rep); err != nil {
					return err
				}
			}
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
func (x *Indexer) finalize(ctx context.Context, fin Header, rep *Report) (int, error) {
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
		// Then payments and cancellations, in chain order, after the deposits that
		// paid for them.
		evRows, err := tx.Query(ctx, `SELECT id FROM withdrawal_events WHERE vault = $1 AND block_number <= $2 AND status = 'pending'
			ORDER BY block_number, log_index`, x.Vault.Bytes(), fin.Number)
		if err != nil {
			return fmt.Errorf("deposits: read pending withdrawal events: %w", err)
		}
		evs, err := pgx.CollectRows(evRows, pgx.RowTo[int64])
		if err != nil {
			return fmt.Errorf("deposits: read pending withdrawal events: %w", err)
		}
		for _, id := range evs {
			if err := x.applyWithdrawal(ctx, tx, in, id, rep); err != nil {
				return err
			}
		}
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

// noteWithdrawal records a payment or a cancellation, applied at once when final.
func (x *Indexer) noteWithdrawal(ctx context.Context, tx pgx.Tx, in *ledger.Ledger, e Event, final bool, rep *Report) error {
	var token, recipient []byte
	var amount *string
	if e.Kind == KindPaid {
		token, recipient = e.Token.Bytes(), e.Recipient.Bytes()
		a := e.Amount.String()
		amount = &a
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO withdrawal_events (vault, block_number, block_hash, log_index, tx_hash, withdrawal_id, kind, token, recipient, amount, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, 'pending') RETURNING id`,
		x.Vault.Bytes(), e.Block, e.BlockHash.Bytes(), e.LogIndex, e.TxHash.Bytes(), e.WithdrawalID.Bytes(), string(e.Kind),
		token, recipient, amount).Scan(&id); err != nil {
		return fmt.Errorf("deposits: record %s %s/%d: %w", e.Kind, e.BlockHash, e.LogIndex, err)
	}
	if final {
		return x.applyWithdrawal(ctx, tx, in, id, rep)
	}
	return nil
}

// applyWithdrawal applies a final payment or cancellation. A payment of a withdrawal
// the ledger holds, to its recipient and for its amount, posts the hold; any other
// payment left the vault without the ledger and goes to suspense. A cancellation of a
// withdrawal the ledger holds releases the hold.
func (x *Indexer) applyWithdrawal(ctx context.Context, tx pgx.Tx, in *ledger.Ledger, eventID int64, rep *Report) error {
	var kind string
	var wid, bh, token, recipient []byte
	var logIndex int
	var amount *string
	if err := tx.QueryRow(ctx, `SELECT kind, withdrawal_id, block_hash, log_index, token, recipient, amount::text FROM withdrawal_events WHERE id = $1`,
		eventID).Scan(&kind, &wid, &bh, &logIndex, &token, &recipient, &amount); err != nil {
		return fmt.Errorf("deposits: read withdrawal event %d: %w", eventID, err)
	}
	var status, ledgerKey, wAmount string
	var wToken, wRecipient []byte
	err := tx.QueryRow(ctx, `SELECT status, token, recipient, amount::text, ledger_key FROM withdrawals WHERE id = $1 AND vault = $2 FOR UPDATE`,
		wid, x.Vault.Bytes()).Scan(&status, &wToken, &wRecipient, &wAmount, &ledgerKey)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("deposits: read withdrawal %x: %w", wid, err)
	}
	open := known && (status == "requested" || status == "onchain")

	outcome := "none"
	switch {
	case kind == string(KindPaid) && open && string(wToken) == string(token) && string(wRecipient) == string(recipient) && wAmount == *amount:
		if _, err := tx.Exec(ctx, "UPDATE withdrawals SET status = 'settled', last_error = NULL WHERE id = $1", wid); err != nil {
			return fmt.Errorf("deposits: settle withdrawal %x: %w", wid, err)
		}
		if _, err := in.Transfer(ctx, ledger.Transfer{Key: ledgerKey + ":post", Kind: ledger.Post, PendingKey: ledgerKey, Code: "withdrawal"}); err != nil {
			return err
		}
		outcome = "settled"
		rep.Settled++
	case kind == string(KindPaid):
		asset := Asset(common.BytesToAddress(token))
		v, err := in.CreateAccount(ctx, "vault:"+asset, asset, ledger.CreditsWithinDebits)
		if err != nil {
			return err
		}
		suspense, err := in.CreateAccount(ctx, SuspenseAccount(asset), asset, ledger.NoLimit)
		if err != nil {
			return err
		}
		amt, _ := new(big.Int).SetString(*amount, 10)
		key := fmt.Sprintf("payout:%x:%x:%d", x.Vault.Bytes(), bh, logIndex)
		if _, err := in.Transfer(ctx, ledger.Transfer{Key: key, Kind: ledger.Single, Debit: suspense.ID, Credit: v.ID, Amount: amt, Code: "payout-unmatched"}); err != nil {
			return err
		}
		outcome = "suspense"
		rep.Suspense++
	case kind == string(KindCancelled) && open:
		if _, err := tx.Exec(ctx, "UPDATE withdrawals SET status = 'cancelled' WHERE id = $1", wid); err != nil {
			return fmt.Errorf("deposits: cancel withdrawal %x: %w", wid, err)
		}
		if _, err := in.Transfer(ctx, ledger.Transfer{Key: ledgerKey + ":void", Kind: ledger.Void, PendingKey: ledgerKey, Code: "withdrawal-cancelled"}); err != nil {
			return err
		}
		outcome = "cancelled"
		rep.Cancelled++
	}
	if _, err := tx.Exec(ctx, "UPDATE withdrawal_events SET status = 'final', outcome = $2 WHERE id = $1", eventID, outcome); err != nil {
		return fmt.Errorf("deposits: finish withdrawal event %d: %w", eventID, err)
	}
	return nil
}

// SuspenseAccount is where payments the ledger never requested are posted: money that
// left the vault without the ledger, kept visible until someone explains it.
func SuspenseAccount(asset string) string { return "suspense:" + asset }
