// Package withdrawals pays customers' withdrawals from the vault exactly once. A
// request holds the customer's balance in the ledger and records the withdrawal under
// an id derived from the request, in one transaction. A payer then brings the vault to
// that id: it reads what the vault records for it, and signs a transaction only when
// the vault has nothing for it, recording the transaction before sending it. The vault
// pays each id once, so retries, crashes and payers running at the same time can only
// repeat a send, never a payment. The deposit indexer settles the hold once the
// payment is final. docs/design/withdrawals.md describes it.
package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
)

// ErrConflict means the request key was used for another withdrawal.
var ErrConflict = errors.New("withdrawals: request key used for another withdrawal")

// Request is a customer's withdrawal.
type Request struct {
	// Key is the caller's idempotency key: the same request sent again with the same key
	// and fields is the same withdrawal.
	Key       string
	Account   common.Hash
	Token     common.Address
	Recipient common.Address
	Amount    *big.Int
}

// Withdrawal is a request as recorded.
type Withdrawal struct {
	ID     common.Hash
	Status string
	Request
}

// ID is the vault's withdrawal id for a request key: keccak256 of a domain, the vault
// and the key. It is the same however often the request is retried, and differs between
// vaults.
func ID(vault common.Address, key string) common.Hash {
	return crypto.Keccak256Hash([]byte("settled withdrawal"), vault.Bytes(), []byte(key))
}

func ledgerKey(id common.Hash) string { return "withdrawal:" + id.Hex() }

// Submit records a withdrawal and holds its amount on the customer's balance, in one
// transaction. A request sent again with the same key returns the withdrawal recorded;
// a different request under that key is ErrConflict; a balance too small is
// ledger.ErrExceedsCredits, and nothing is recorded.
func Submit(ctx context.Context, l *ledger.Ledger, vault common.Address, r Request) (Withdrawal, error) {
	if r.Key == "" || r.Amount == nil || r.Amount.Sign() <= 0 || r.Recipient == (common.Address{}) {
		return Withdrawal{}, errors.New("withdrawals: a request needs a key, a positive amount and a recipient")
	}
	id := ID(vault, r.Key)
	w := Withdrawal{ID: id, Request: r}
	err := l.InTx(ctx, func(tx pgx.Tx, in *ledger.Ledger) error {
		asset := deposits.Asset(r.Token)
		v, err := in.CreateAccount(ctx, "vault:"+asset, asset, ledger.CreditsWithinDebits)
		if err != nil {
			return err
		}
		c, err := in.CreateAccount(ctx, deposits.CustomerAccount(r.Account, asset), asset, ledger.DebitsWithinCredits)
		if err != nil {
			return err
		}
		// Two requests with the same key at once: the second waits for the first here and
		// then finds its row.
		tag, err := tx.Exec(ctx, `INSERT INTO withdrawals (id, vault, request_key, account, token, recipient, amount, ledger_key, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, 'requested') ON CONFLICT (request_key) DO NOTHING`,
			id.Bytes(), vault.Bytes(), r.Key, r.Account.Bytes(), r.Token.Bytes(), r.Recipient.Bytes(), r.Amount.String(), ledgerKey(id))
		if err != nil {
			return fmt.Errorf("withdrawals: record request %s: %w", r.Key, err)
		}
		if tag.RowsAffected() == 0 {
			var account, token, recipient, gotID []byte
			var amount string
			if err := tx.QueryRow(ctx, `SELECT id, status, account, token, recipient, amount::text FROM withdrawals WHERE request_key = $1`, r.Key).
				Scan(&gotID, &w.Status, &account, &token, &recipient, &amount); err != nil {
				return fmt.Errorf("withdrawals: read request %s: %w", r.Key, err)
			}
			a, _ := new(big.Int).SetString(amount, 10)
			if common.BytesToHash(gotID) != id || common.BytesToHash(account) != r.Account || common.BytesToAddress(token) != r.Token ||
				common.BytesToAddress(recipient) != r.Recipient || a.Cmp(r.Amount) != 0 {
				return fmt.Errorf("%w: %s", ErrConflict, r.Key)
			}
			return nil
		}
		if _, err := in.Transfer(ctx, ledger.Transfer{Key: ledgerKey(id), Kind: ledger.Pending, Debit: c.ID, Credit: v.ID, Amount: r.Amount, Code: "withdrawal"}); err != nil {
			return err
		}
		w.Status = "requested"
		return nil
	})
	if err != nil {
		return Withdrawal{}, err
	}
	return w, nil
}
