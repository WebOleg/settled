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

// Signer signs the payer's transactions; the payer never holds the key itself. The
// withdrawal signer (internal/signer) signs only what its policy and the ledger allow,
// and refuses the rest with a *Refused.
type Signer interface {
	// Address is the account it signs for: the one that holds the vault's operator role.
	Address() common.Address
	// Sign returns tx signed, or a *Refused when its policy does not allow it.
	Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error)
}

// Refused is a signer's refusal: the rule of its policy that the transaction breaks,
// and why. The payer signs nothing for it, takes no nonce, and records the reason.
type Refused struct {
	Rule, Reason string
}

func (r *Refused) Error() string { return fmt.Sprintf("signer refused (%s): %s", r.Rule, r.Reason) }

// KeySigner signs with a key held in this process, with no policy of its own: for tests
// and the end-to-end runs, where the payer's rules are what is under test.
type KeySigner struct {
	Key *ecdsa.PrivateKey
}

func (k KeySigner) Address() common.Address { return crypto.PubkeyToAddress(k.Key.PublicKey) }

func (k KeySigner) Sign(_ context.Context, tx *types.Transaction) (*types.Transaction, error) {
	return types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), k.Key)
}

// Payer pays the open withdrawals of one vault through a signer that holds the
// operator's key. Several payers may run at once for the same key: each withdrawal is
// worked on by one at a time (row locks, skipped when taken) and nonces come from one
// row in the database.
type Payer struct {
	Chain  Chain
	Ledger *ledger.Ledger
	Vault  common.Address
	Signer Signer
	// ResendAfter is how long a sent transaction may go unmined before it is sent again,
	// the same bytes.
	ResendAfter time.Duration
	// BumpAfter is how long the latest version of a transaction may go unmined before it
	// is replaced: the same call under the same nonce, with fees an eighth higher, or the
	// network's own if they are higher still. Zero never replaces.
	BumpAfter time.Duration
	// MaxFeeCap is the most a transaction may offer per gas: a replacement never goes
	// above it, and nothing is signed while the network asks for more. Required.
	MaxFeeCap *big.Int
	// Batch is the most withdrawals one step works on.
	Batch int
	// beforeSend, in tests, runs after a transaction is recorded and before it is sent.
	beforeSend func(*types.Transaction) error
}

// Report is what one step did.
type Report struct {
	Signed, Resent, OnChain, Rejected, Reopened, Waiting int
	// Bumped are replacements signed; Capped are transactions left as they are because
	// a replacement would offer more than MaxFeeCap; Refused are transactions the signer
	// would not sign.
	Bumped, Capped, Refused int
	// Closed are transactions closed: a version final, or the nonce used by a final
	// block.
	Closed int
}

func (p *Payer) sender() common.Address { return p.Signer.Address() }

// Step works once on every open withdrawal: it brings the vault to each, signing a
// transaction only where the vault has nothing for the id yet.
func (p *Payer) Step(ctx context.Context) (Report, error) {
	var rep Report
	if p.MaxFeeCap == nil || p.MaxFeeCap.Sign() <= 0 {
		return rep, errors.New("withdrawals: the payer needs a MaxFeeCap")
	}
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
	// A withdrawal settled, cancelled or rejected may still have transactions open: the
	// indexer settles it once its payment is final, and the payer has not looked since.
	// They are closed the same way, and one still in flight is sent again, so its nonce
	// is not left empty.
	rows, err = p.Ledger.Pool().Query(ctx, `SELECT DISTINCT t.withdrawal_id FROM withdrawal_txs t JOIN withdrawals w ON w.id = t.withdrawal_id
		WHERE w.vault = $1 AND NOT t.done AND w.status NOT IN ('requested', 'onchain') LIMIT $2`, p.Vault.Bytes(), max(p.Batch, 1))
	if err != nil {
		return rep, fmt.Errorf("withdrawals: list closed with transactions open: %w", err)
	}
	done, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (common.Hash, error) {
		var b []byte
		err := r.Scan(&b)
		return common.BytesToHash(b), err
	})
	if err != nil {
		return rep, fmt.Errorf("withdrawals: list closed with transactions open: %w", err)
	}
	for _, id := range done {
		if err := p.tidy(ctx, id, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// sweep closes the slots of a withdrawal that no reorganisation can undo any more: a
// version mined in a final block, or a nonce a final block has used. Until then a
// reorganisation can take a transaction out of the chain again. It returns the slots in
// flight: open, with no version mined now.
func (p *Payer) sweep(ctx context.Context, tx pgx.Tx, id common.Hash, rep *Report) ([]slot, error) {
	slots, err := p.openSlots(ctx, tx, id)
	if err != nil || len(slots) == 0 {
		return nil, err
	}
	mined, err := p.Chain.Nonce(ctx, p.sender())
	if err != nil {
		return nil, err
	}
	finalBlock, finalNonce, err := p.Chain.Finalized(ctx, p.sender())
	if err != nil {
		return nil, err
	}
	var inFlight []slot
	for _, s := range slots {
		var r *Receipt
		for _, o := range s.versions {
			if r, err = p.Chain.Receipt(ctx, o.hash); err != nil {
				return nil, err
			}
			if r != nil {
				break
			}
		}
		if (r != nil && r.Block <= finalBlock) || (r == nil && s.nonce < finalNonce) {
			for _, o := range s.versions {
				if _, err := tx.Exec(ctx, "UPDATE withdrawal_txs SET done = true WHERE id = $1", o.id); err != nil {
					return nil, fmt.Errorf("withdrawals: close tx %s: %w", o.hash, err)
				}
				rep.Closed++
			}
			continue
		}
		if r == nil && s.nonce >= mined {
			inFlight = append(inFlight, s)
		}
	}
	return inFlight, nil
}

// tidy closes what it can of the transactions of a withdrawal no longer open, and sends
// one still in flight again, its latest version; it signs nothing new.
func (p *Payer) tidy(ctx context.Context, id common.Hash, rep *Report) error {
	var toSend []*types.Transaction
	err := p.Ledger.InTx(ctx, func(tx pgx.Tx, _ *ledger.Ledger) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM withdrawals WHERE id = $1 FOR UPDATE SKIP LOCKED", id.Bytes()).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another payer has it
		}
		if err != nil {
			return fmt.Errorf("withdrawals: lock %s: %w", id, err)
		}
		inFlight, err := p.sweep(ctx, tx, id, rep)
		if err != nil {
			return err
		}
		for _, s := range inFlight {
			o := s.latest()
			if o.sinceSent == nil || *o.sinceSent >= p.ResendAfter {
				t := new(types.Transaction)
				if err := t.UnmarshalBinary(o.raw); err != nil {
					return fmt.Errorf("withdrawals: decode tx %s: %w", o.hash, err)
				}
				toSend = append(toSend, t)
				rep.Resent++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return p.send(ctx, toSend)
}

// open is one version of an open transaction.
type open struct {
	id    int64
	kind  string
	nonce uint64
	hash  common.Hash
	raw   []byte
	// age is how long ago it was recorded; sinceSent how long ago it was last sent, nil
	// if never. Both by the database's clock.
	age       time.Duration
	sinceSent *time.Duration
}

// slot is an open nonce: its versions in the order they were signed, the latest last.
type slot struct {
	nonce    uint64
	versions []open
}

func (s slot) latest() open { return s.versions[len(s.versions)-1] }

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

		inFlight, err := p.sweep(ctx, tx, id, rep)
		if err != nil {
			return err
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
			status = s
			return nil
		}
		// resend looks after the transaction of kind in flight, if there is one: replaces
		// it when it has waited too long, else sends it again when due. Nothing new is
		// signed while one is in flight.
		resend := func(kind string) (bool, error) {
			for _, s := range inFlight {
				o := s.latest()
				if o.kind != kind {
					continue
				}
				if p.BumpAfter > 0 && o.age >= p.BumpAfter {
					// Not replaced, at the fee cap or refused by the signer: the reason is
					// recorded, and the latest version is sent again when due.
					t, why, err := p.replace(ctx, tx, id, o)
					switch {
					case errors.Is(err, errRefused):
						rep.Refused++
						if err := setStatus(status, why); err != nil {
							return true, err
						}
					case err != nil:
						return true, err
					case t != nil:
						toSend = append(toSend, t)
						rep.Bumped++
						return true, nil
					default:
						rep.Capped++
						if err := setStatus(status, why); err != nil {
							return true, err
						}
					}
				}
				if o.sinceSent == nil || *o.sinceSent >= p.ResendAfter {
					t := new(types.Transaction)
					if err := t.UnmarshalBinary(o.raw); err != nil {
						return true, fmt.Errorf("withdrawals: decode tx %s: %w", o.hash, err)
					}
					toSend = append(toSend, t)
					rep.Resent++
				}
				return true, nil
			}
			return false, nil
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
			if head+1 < onChain.ReleaseAt {
				rep.Waiting++
				return nil
			}
			if busy, err := resend("execute"); err != nil || busy {
				if err == nil {
					rep.Waiting++
				}
				return err
			}
			data, err := VaultABI.Pack("executeQueued", id)
			if err != nil {
				return err
			}
			t, lastErr, err := p.sign(ctx, tx, id, "execute", data)
			if errors.Is(err, errRefused) {
				rep.Refused++
				rep.Waiting++
				return setStatus("onchain", lastErr)
			}
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
		if busy, err := resend("withdraw"); err != nil || busy {
			return err
		}
		data, err := VaultABI.Pack("withdraw", id, common.BytesToAddress(token), common.BytesToAddress(recipient), amt)
		if err != nil {
			return err
		}
		t, lastErr, err := p.sign(ctx, tx, id, "withdraw", data)
		if errors.Is(err, errRefused) {
			rep.Refused++
			rep.Waiting++
			return setStatus("requested", lastErr)
		}
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
	return p.send(ctx, toSend)
}

// send sends what a step recorded, after its commit. A node that already has the
// transaction, or has mined its nonce, says so, which is fine: the next step reads the
// receipt.
func (p *Payer) send(ctx context.Context, toSend []*types.Transaction) error {
	for _, t := range toSend {
		if p.beforeSend != nil {
			if err := p.beforeSend(t); err != nil {
				return err
			}
		}
		if err := p.Chain.Send(ctx, t); err != nil && !alreadyKnown(err) {
			return fmt.Errorf("withdrawals: send %s: %w", t.Hash(), err)
		}
		if _, err := p.Ledger.Pool().Exec(ctx, "UPDATE withdrawal_txs SET last_sent_at = now() WHERE hash = $1", t.Hash().Bytes()); err != nil {
			return fmt.Errorf("withdrawals: note send of %s: %w", t.Hash(), err)
		}
	}
	return nil
}

// errRefused marks a refusal of the signer, whose reason comes with it.
var errRefused = errors.New("withdrawals: refused by the signer")

// signed checks that what the signer returned is what was asked for, signed by the
// payer's account: the same chain, nonce, fees, gas, destination, value and data.
func (p *Payer) signed(asked, got *types.Transaction) error {
	from, err := types.Sender(types.LatestSignerForChainID(asked.ChainId()), got)
	if err != nil || from != p.sender() || got.Type() != asked.Type() || got.ChainId().Cmp(asked.ChainId()) != 0 ||
		got.Nonce() != asked.Nonce() || got.GasTipCap().Cmp(asked.GasTipCap()) != 0 || got.GasFeeCap().Cmp(asked.GasFeeCap()) != 0 ||
		got.Gas() != asked.Gas() || got.To() == nil || *got.To() != *asked.To() || got.Value().Cmp(asked.Value()) != 0 ||
		string(got.Data()) != string(asked.Data()) || len(got.AccessList()) != 0 {
		return fmt.Errorf("withdrawals: the signer returned a transaction other than the one asked for")
	}
	return nil
}

func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already known") || strings.Contains(s, "nonce too low") || strings.Contains(s, "already imported")
}

// openSlots reads the open transactions of a withdrawal, grouped by nonce.
func (p *Payer) openSlots(ctx context.Context, tx pgx.Tx, id common.Hash) ([]slot, error) {
	rows, err := tx.Query(ctx, `SELECT id, kind, nonce, hash, raw, extract(epoch FROM now() - created_at)::float8,
		extract(epoch FROM now() - last_sent_at)::float8 FROM withdrawal_txs
		WHERE withdrawal_id = $1 AND NOT done ORDER BY nonce, id`, id.Bytes())
	if err != nil {
		return nil, fmt.Errorf("withdrawals: read txs of %s: %w", id, err)
	}
	versions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (open, error) {
		var o open
		var hash []byte
		var age float64
		var sinceSent *float64
		err := r.Scan(&o.id, &o.kind, &o.nonce, &hash, &o.raw, &age, &sinceSent)
		o.hash = common.BytesToHash(hash)
		o.age = seconds(age)
		if sinceSent != nil {
			d := seconds(*sinceSent)
			o.sinceSent = &d
		}
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("withdrawals: read txs of %s: %w", id, err)
	}
	var slots []slot
	for _, o := range versions {
		if n := len(slots); n > 0 && slots[n-1].nonce == o.nonce {
			slots[n-1].versions = append(slots[n-1].versions, o)
			continue
		}
		slots = append(slots, slot{nonce: o.nonce, versions: []open{o}})
	}
	return slots, nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// bumped is x raised by an eighth, rounded up: above the tenth nodes require for a
// replacement, with room for the base fee to rise a block's worth meanwhile.
func bumped(x *big.Int) *big.Int {
	b := new(big.Int).Mul(x, big.NewInt(9))
	b.Add(b, big.NewInt(7))
	return b.Div(b, big.NewInt(8))
}

// atLeastATenthMore is the rule the database holds a replacement to.
func atLeastATenthMore(n, old *big.Int) bool {
	return new(big.Int).Mul(n, big.NewInt(10)).Cmp(new(big.Int).Mul(old, big.NewInt(11))) >= 0
}

func bigMax(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// replace signs and records a replacement of o: the same call, gas and nonce, with
// fees raised by an eighth, or the network's if higher, and never above MaxFeeCap.
// When MaxFeeCap leaves no room for a tenth more, it returns no transaction and why.
func (p *Payer) replace(ctx context.Context, tx pgx.Tx, id common.Hash, o open) (*types.Transaction, string, error) {
	old := new(types.Transaction)
	if err := old.UnmarshalBinary(o.raw); err != nil {
		return nil, "", fmt.Errorf("withdrawals: decode tx %s: %w", o.hash, err)
	}
	tip, feeCap, err := p.Chain.Fees(ctx)
	if err != nil {
		return nil, "", err
	}
	newTip := bigMax(bumped(old.GasTipCap()), tip)
	newCap := bigMax(bumped(old.GasFeeCap()), feeCap)
	newCap = bigMax(newCap, newTip)
	if newCap.Cmp(p.MaxFeeCap) > 0 {
		newCap = new(big.Int).Set(p.MaxFeeCap)
		if newTip.Cmp(newCap) > 0 {
			newTip = new(big.Int).Set(newCap)
		}
	}
	if !atLeastATenthMore(newCap, old.GasFeeCap()) || !atLeastATenthMore(newTip, old.GasTipCap()) {
		return nil, fmt.Sprintf("unmined at fee cap %s wei, and MaxFeeCap %s leaves no room to replace it", old.GasFeeCap(), p.MaxFeeCap), nil
	}
	chainID, err := p.Chain.ChainID(ctx)
	if err != nil {
		return nil, "", err
	}
	asked := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: old.Nonce(), GasTipCap: newTip, GasFeeCap: newCap,
		Gas: old.Gas(), To: old.To(), Data: old.Data(),
	})
	t, err := p.Signer.Sign(ctx, asked)
	var refused *Refused
	if errors.As(err, &refused) {
		return nil, refused.Error(), errRefused
	}
	if err != nil {
		return nil, "", fmt.Errorf("withdrawals: sign a replacement: %w", err)
	}
	if err := p.signed(asked, t); err != nil {
		return nil, "", err
	}
	raw, err := t.MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_txs (withdrawal_id, kind, sender, nonce, hash, raw, tip_cap, fee_cap, data, replaces)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9, $10)`,
		id.Bytes(), o.kind, p.sender().Bytes(), t.Nonce(), t.Hash().Bytes(), raw, newTip.String(), newCap.String(), t.Data(), o.id); err != nil {
		return nil, "", fmt.Errorf("withdrawals: record the replacement of %s: %w", o.hash, err)
	}
	return t, "", nil
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
	// A network asking more than MaxFeeCap is waited out, before a nonce is taken.
	tip, feeCap, err := p.Chain.Fees(ctx)
	if err != nil {
		return nil, "", err
	}
	if feeCap.Cmp(p.MaxFeeCap) > 0 {
		return nil, fmt.Sprintf("the network asks a fee cap of %s wei, above MaxFeeCap %s", feeCap, p.MaxFeeCap), nil
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
	asked := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: next, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: gas + gas/5, To: &p.Vault, Data: data,
	})
	t, err := p.Signer.Sign(ctx, asked)
	var refused *Refused
	if errors.As(err, &refused) {
		// Nothing is recorded and the nonce is not taken: the next step asks again.
		return nil, refused.Error(), errRefused
	}
	if err != nil {
		return nil, "", fmt.Errorf("withdrawals: sign: %w", err)
	}
	if err := p.signed(asked, t); err != nil {
		return nil, "", err
	}
	raw, err := t.MarshalBinary()
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE payer_nonces SET next = $2 WHERE sender = $1", from.Bytes(), next+1); err != nil {
		return nil, "", fmt.Errorf("withdrawals: advance the nonce: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_txs (withdrawal_id, kind, sender, nonce, hash, raw, tip_cap, fee_cap, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8::numeric, $9)`,
		id.Bytes(), kind, from.Bytes(), next, t.Hash().Bytes(), raw, tip.String(), feeCap.String(), data); err != nil {
		return nil, "", fmt.Errorf("withdrawals: record tx: %w", err)
	}
	return t, "", nil
}
