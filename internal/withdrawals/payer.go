package withdrawals

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/ledger"
)

// permanent are the vault's refusals that no retry will change: the hold is released
// and the withdrawal rejected. Any other refusal (rate limit, pause, the vault short of
// funds) is waited out.
var permanent = map[string]bool{
	"NativeTransferFailed": true,
	"TokenNotAllowed":      true,
	"ZeroAddress":          true,
	"ZeroAmount":           true,
}

// Payer pays the open withdrawals of one vault with the operator's key. Several payers
// may run at once with the same key: each withdrawal is worked on by one at a time
// (row locks, skipped when taken) and nonces come from one row in the database.
type Payer struct {
	Chain  Chain
	Ledger *ledger.Ledger
	Vault  common.Address
	Key    *ecdsa.PrivateKey
	// ResendAfter is how long a sent transaction may go unmined before it is sent again,
	// the same bytes.
	ResendAfter time.Duration
	// Batch is the most withdrawals one step works on.
	Batch int
	// beforeSend, in tests, runs after a transaction is recorded and before it is sent.
	beforeSend func(*types.Transaction) error
}

// Report is what one step did.
type Report struct {
	Signed, Resent, OnChain, Rejected, Reopened, Waiting int
}

func (p *Payer) sender() common.Address { return crypto.PubkeyToAddress(p.Key.PublicKey) }

// Step works once on every open withdrawal: it brings the vault to each, signing a
// transaction only where the vault has nothing for the id yet.
func (p *Payer) Step(ctx context.Context) (Report, error) {
	var rep Report
	rows, err := p.Ledger.Pool().Query(ctx, `SELECT id FROM withdrawals WHERE vault = $1 AND status IN ('requested', 'onchain')
		ORDER BY created_at LIMIT $2`, p.Vault.Bytes(), max(p.Batch, 1))
	if err != nil {
		return rep, fmt.Errorf("withdrawals: list open: %w", err)
	}
	ids, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (common.Hash, error) {
		var b []byte
		err := r.Scan(&b)
		return common.BytesToHash(b), err
	})
	if err != nil {
		return rep, fmt.Errorf("withdrawals: list open: %w", err)
	}
	for _, id := range ids {
		if err := p.work(ctx, id, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

type open struct {
	id       int64
	kind     string
	nonce    uint64
	hash     common.Hash
	raw      []byte
	lastSent *time.Time
}

// work brings the vault to one withdrawal, in one transaction that holds its row.
// What must be sent is sent after the commit, so a transaction is always recorded
// before anyone can see it.
func (p *Payer) work(ctx context.Context, id common.Hash, rep *Report) error {
	var toSend []*types.Transaction
	err := p.Ledger.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
		var status, ledgerKey string
		var token, recipient []byte
		var amount string
		err := tx.QueryRow(ctx, `SELECT status, token, recipient, amount::text, ledger_key FROM withdrawals
			WHERE id = $1 AND status IN ('requested', 'onchain') FOR UPDATE SKIP LOCKED`, id.Bytes()).
			Scan(&status, &token, &recipient, &amount, &ledgerKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another payer has it, or it is done
		}
		if err != nil {
			return fmt.Errorf("withdrawals: lock %s: %w", id, err)
		}
		amt, _ := new(big.Int).SetString(amount, 10)

		// A transaction stays open until no reorganisation can undo it: mined in a final
		// block, or its nonce used by a final block. Until then a reorganisation can take
		// it out of the chain again, and it is sent again, the same bytes, so its nonce is
		// never left empty. In flight is open and not mined now.
		pending, err := p.openTxs(ctx, tx, id)
		if err != nil {
			return err
		}
		mined, err := p.Chain.Nonce(ctx, p.sender())
		if err != nil {
			return err
		}
		finalBlock, finalNonce, err := p.Chain.Finalized(ctx, p.sender())
		if err != nil {
			return err
		}
		var inFlight []open
		for _, o := range pending {
			r, err := p.Chain.Receipt(ctx, o.hash)
			if err != nil {
				return err
			}
			if (r != nil && r.Block <= finalBlock) || (r == nil && o.nonce < finalNonce) {
				if _, err := tx.Exec(ctx, "UPDATE withdrawal_txs SET done = true WHERE id = $1", o.id); err != nil {
					return fmt.Errorf("withdrawals: close tx %s: %w", o.hash, err)
				}
				continue
			}
			if r == nil && o.nonce >= mined {
				inFlight = append(inFlight, o)
			}
		}

		onChain, err := p.Chain.Withdrawal(ctx, p.Vault, id)
		if err != nil {
			return err
		}
		setStatus := func(s, lastErr string) error {
			_, err := tx.Exec(ctx, "UPDATE withdrawals SET status = $2, last_error = NULLIF($3, '') WHERE id = $1", id.Bytes(), s, lastErr)
			if err != nil {
				return fmt.Errorf("withdrawals: %s to %s: %w", id, s, err)
			}
			return nil
		}
		resend := func(kind string) bool {
			for _, o := range inFlight {
				if o.kind != kind {
					continue
				}
				if o.lastSent == nil || time.Since(*o.lastSent) >= p.ResendAfter {
					t := new(types.Transaction)
					if err := t.UnmarshalBinary(o.raw); err == nil {
						toSend = append(toSend, t)
						rep.Resent++
					}
				}
				return true // one is in flight; nothing new is signed
			}
			return false
		}

		switch onChain.Status {
		case Paid, Cancelled:
			if status == "requested" {
				rep.OnChain++
				return setStatus("onchain", "")
			}
			return nil
		case Queued:
			if status == "requested" {
				rep.OnChain++
				if err := setStatus("onchain", ""); err != nil {
					return err
				}
			}
			head, err := p.Chain.Head(ctx)
			if err != nil {
				return err
			}
			// The vault pays a queued withdrawal from its release block on.
			if head+1 < onChain.ReleaseAt || resend("execute") {
				rep.Waiting++
				return nil
			}
			data, err := VaultABI.Pack("executeQueued", id)
			if err != nil {
				return err
			}
			t, lastErr, err := p.sign(ctx, tx, id, "execute", data)
			var rv *Revert
			if errors.As(err, &rv) {
				// The vault will not pay it, a recipient that refuses ETH say: it waits in
				// the queue for the guardian to cancel, which releases the hold.
				rep.Waiting++
				return setStatus("onchain", rv.Error())
			}
			if err != nil {
				return err
			}
			if t == nil {
				rep.Waiting++
				return setStatus("onchain", lastErr)
			}
			toSend = append(toSend, t)
			rep.Signed++
			return nil
		}

		// The vault has nothing for the id.
		if status == "onchain" {
			// It had: a reorganisation took it away. Pay it again.
			rep.Reopened++
			if err := setStatus("requested", "the vault's record of it left the chain"); err != nil {
				return err
			}
		}
		if resend("withdraw") {
			return nil
		}
		data, err := VaultABI.Pack("withdraw", id, common.BytesToAddress(token), common.BytesToAddress(recipient), amt)
		if err != nil {
			return err
		}
		t, lastErr, err := p.sign(ctx, tx, id, "withdraw", data)
		var rv *Revert
		if errors.As(err, &rv) && permanent[rv.Name] {
			rep.Rejected++
			if err := setStatus("rejected", rv.Error()); err != nil {
				return err
			}
			_, err := in.Transfer(ctx, ledger.Transfer{Key: ledgerKey + ":void", Kind: ledger.Void, PendingKey: ledgerKey, Code: "withdrawal-rejected"})
			return err
		}
		if err != nil {
			return err
		}
		if t == nil {
			rep.Waiting++
			return setStatus("requested", lastErr)
		}
		toSend = append(toSend, t)
		rep.Signed++
		return nil
	})
	if err != nil {
		return err
	}
	for _, t := range toSend {
		if p.beforeSend != nil {
			if err := p.beforeSend(t); err != nil {
				return err
			}
		}
		// A node that already has it, or has mined its nonce, says so: either is fine,
		// the next step reads the receipt.
		if err := p.Chain.Send(ctx, t); err != nil && !alreadyKnown(err) {
			return fmt.Errorf("withdrawals: send %s: %w", t.Hash(), err)
		}
		if _, err := p.Ledger.Pool().Exec(ctx, "UPDATE withdrawal_txs SET last_sent_at = now() WHERE hash = $1", t.Hash().Bytes()); err != nil {
			return fmt.Errorf("withdrawals: note send of %s: %w", t.Hash(), err)
		}
	}
	return nil
}

func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") || strings.Contains(s, "nonce too low") || strings.Contains(s, "already imported")
}

func (p *Payer) openTxs(ctx context.Context, tx pgx.Tx, id common.Hash) ([]open, error) {
	rows, err := tx.Query(ctx, `SELECT id, kind, nonce, hash, raw, last_sent_at FROM withdrawal_txs
		WHERE withdrawal_id = $1 AND NOT done ORDER BY id`, id.Bytes())
	if err != nil {
		return nil, fmt.Errorf("withdrawals: read txs of %s: %w", id, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (open, error) {
		var o open
		var hash []byte
		err := r.Scan(&o.id, &o.kind, &o.nonce, &hash, &o.raw, &o.lastSent)
		o.hash = common.BytesToHash(hash)
		return o, err
	})
}

// sign checks data against the vault without sending it, then signs it with the next
// nonce and records it. A refusal the payer waits out returns no transaction and the
// reason; a permanent one returns the *Revert.
func (p *Payer) sign(ctx context.Context, tx pgx.Tx, id common.Hash, kind string, data []byte) (*types.Transaction, string, error) {
	from := p.sender()
	gas, err := p.Chain.Estimate(ctx, from, p.Vault, data)
	var rv *Revert
	if errors.As(err, &rv) {
		if permanent[rv.Name] {
			return nil, "", rv
		}
		return nil, rv.Error(), nil
	}
	if err != nil {
		return nil, "", err
	}
	// The next nonce: the row is locked, so payers that share the key take turns; the
	// chain's nonce wins when something else used the key.
	mined, err := p.Chain.Nonce(ctx, from)
	if err != nil {
		return nil, "", err
	}
	var next uint64
	err = tx.QueryRow(ctx, `INSERT INTO payer_nonces (sender, next) VALUES ($1, $2)
		ON CONFLICT (sender) DO UPDATE SET next = greatest(payer_nonces.next, EXCLUDED.next)
		RETURNING next`, from.Bytes(), mined).Scan(&next)
	if err != nil {
		return nil, "", fmt.Errorf("withdrawals: take a nonce: %w", err)
	}
	chainID, err := p.Chain.ChainID(ctx)
	if err != nil {
		return nil, "", err
	}
	tip, feeCap, err := p.Chain.Fees(ctx)
	if err != nil {
		return nil, "", err
	}
	t, err := types.SignNewTx(p.Key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: next, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: gas + gas/5, To: &p.Vault, Data: data,
	})
	if err != nil {
		return nil, "", fmt.Errorf("withdrawals: sign: %w", err)
	}
	raw, err := t.MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE payer_nonces SET next = $2 WHERE sender = $1", from.Bytes(), next+1); err != nil {
		return nil, "", fmt.Errorf("withdrawals: advance the nonce: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_txs (withdrawal_id, kind, sender, nonce, hash, raw) VALUES ($1, $2, $3, $4, $5, $6)`,
		id.Bytes(), kind, from.Bytes(), next, t.Hash().Bytes(), raw); err != nil {
		return nil, "", fmt.Errorf("withdrawals: record tx: %w", err)
	}
	return t, "", nil
}
