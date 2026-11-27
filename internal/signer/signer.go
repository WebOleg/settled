// Package signer is the withdrawal signer: the only holder of the payer's key. The
// payer (internal/withdrawals) asks it to sign; it signs only what its policy allows and
// what the ledger holds, the way a custodian's policy engine stands between a hot wallet
// and its key:
//
//   - the transaction is an EIP-1559 one on the chain it serves, to the vault, with no
//     value and no access list, within a gas limit and a fee cap;
//   - it calls withdraw or executeQueued, and nothing else;
//   - the withdrawal it names is open in the ledger, and withdraw pays exactly the
//     ledger's token, recipient and amount;
//   - what it has signed for withdraw in the last window, this one included, stays
//     within a limit per token (a velocity limit).
//
// Each decision, signed or refused, is recorded in signer_log before the answer leaves,
// in the same database transaction, so there is no signature without a record. The
// signer reads the ledger with a login of its own and can change nothing in it.
// docs/design/signer.md describes it.
package signer

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/withdrawals"
)

// Key signs a hash for one account. A key in a file is one; a key in a KMS or an HSM,
// which never leaves it, is another behind the same two calls.
type Key interface {
	Address() common.Address
	// SignHash returns the 65-byte [R || S || V] signature of hash, V being 0 or 1.
	SignHash(ctx context.Context, hash common.Hash) ([]byte, error)
}

// FileKey is a key read from a file, held in the signer's process only.
type FileKey struct{ key *ecdsa.PrivateKey }

// NewFileKey wraps a key held in memory.
func NewFileKey(key *ecdsa.PrivateKey) FileKey { return FileKey{key: key} }

// ReadFileKey reads a hex private key, with or without 0x, from a file. The file's
// content is never repeated in an error.
func ReadFileKey(path string) (FileKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return FileKey{}, fmt.Errorf("signer: read the key: %w", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil {
		return FileKey{}, errors.New("signer: the key file does not hold a hex private key")
	}
	return FileKey{key: key}, nil
}

func (k FileKey) Address() common.Address { return crypto.PubkeyToAddress(k.key.PublicKey) }

func (k FileKey) SignHash(_ context.Context, hash common.Hash) ([]byte, error) {
	return crypto.Sign(hash.Bytes(), k.key)
}

// Policy is what the signer allows.
type Policy struct {
	ChainID   *big.Int
	Vault     common.Address
	MaxGas    uint64
	MaxFeeCap *big.Int
	// Window and Limit are the velocity limit: what withdraw may pay, per token, over
	// any Window, each withdrawal counted once however often it is signed again.
	Window time.Duration
	Limit  *big.Int
}

// Service signs under a policy, recording each decision in signer_log.
type Service struct {
	Key    Key
	Policy Policy
	// DB connects as a login that holds withdrawal_signer.
	DB *pgxpool.Pool
	// Observe, when set, is told each decision once it is recorded.
	Observe func(decision, rule string)
}

func (s *Service) observe(decision, rule string) {
	if s.Observe != nil {
		s.Observe(decision, rule)
	}
}

// record is one row of signer_log.
type record struct {
	client, decision, rule, reason string
	tx                             *types.Transaction
	method                         string
	id                             *common.Hash
	token, recipient               *common.Address
	amount                         *big.Int
	hash                           *common.Hash
}

// Sign signs tx for client, or refuses it with a *withdrawals.Refused naming the rule
// it breaks. Either way the decision is recorded first.
func (s *Service) Sign(ctx context.Context, client string, tx *types.Transaction) (*types.Transaction, error) {
	var signed *types.Transaction
	var refusal *withdrawals.Refused
	err := pgx.BeginFunc(ctx, s.DB, func(db pgx.Tx) error {
		// One decision at a time, across every signer that serves the key, so the
		// velocity limit is read and spent atomically.
		if _, err := db.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('settled: withdrawal signer'))"); err != nil {
			return fmt.Errorf("signer: lock: %w", err)
		}
		r := record{client: client, tx: tx}
		rule, reason, err := s.check(ctx, db, tx, &r)
		if err != nil {
			return err
		}
		if rule != "" {
			r.decision, r.rule, r.reason = "refused", rule, reason
			refusal = &withdrawals.Refused{Rule: rule, Reason: reason}
			return s.write(ctx, db, r)
		}
		signer := types.LatestSignerForChainID(s.Policy.ChainID)
		sig, err := s.Key.SignHash(ctx, signer.Hash(tx))
		if err != nil {
			return fmt.Errorf("signer: sign: %w", err)
		}
		t, err := tx.WithSignature(signer, sig)
		if err != nil {
			return fmt.Errorf("signer: attach the signature: %w", err)
		}
		if from, err := types.Sender(signer, t); err != nil || from != s.Key.Address() {
			return errors.New("signer: the key signed for another account")
		}
		h := t.Hash()
		r.decision, r.rule, r.hash = "signed", "allowed", &h
		if err := s.write(ctx, db, r); err != nil {
			return err
		}
		signed = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		s.observe("refused", refusal.Rule)
		return nil, refusal
	}
	s.observe("signed", "allowed")
	return signed, nil
}

// check applies the policy. It returns the rule tx breaks and why, or no rule.
func (s *Service) check(ctx context.Context, db pgx.Tx, tx *types.Transaction, r *record) (string, string, error) {
	p := s.Policy
	switch {
	case tx.Type() != types.DynamicFeeTxType:
		return "type", fmt.Sprintf("transaction type %d, only EIP-1559 is signed", tx.Type()), nil
	case tx.ChainId().Cmp(p.ChainID) != 0:
		return "chain", fmt.Sprintf("chain %s, this signer serves %s", tx.ChainId(), p.ChainID), nil
	case tx.To() == nil || *tx.To() != p.Vault:
		return "destination", "only calls to the vault are signed", nil
	case tx.Value().Sign() != 0:
		return "value", fmt.Sprintf("%s wei of value; calls to the vault carry none", tx.Value()), nil
	case len(tx.AccessList()) != 0:
		return "access list", "an access list is not signed", nil
	case tx.Gas() > p.MaxGas:
		return "gas", fmt.Sprintf("gas %d, at most %d", tx.Gas(), p.MaxGas), nil
	case tx.GasFeeCap().Cmp(p.MaxFeeCap) > 0:
		return "fee cap", fmt.Sprintf("fee cap %s wei, at most %s", tx.GasFeeCap(), p.MaxFeeCap), nil
	case tx.GasTipCap().Cmp(tx.GasFeeCap()) > 0:
		return "fee cap", "the tip is above the fee cap", nil
	}
	data := tx.Data()
	if len(data) < 4 {
		return "method", "no call", nil
	}
	m, err := withdrawals.VaultABI.MethodById(data[:4])
	if err != nil || (m.Name != "withdraw" && m.Name != "executeQueued") {
		return "method", "only withdraw and executeQueued are signed", nil
	}
	args, err := m.Inputs.Unpack(data[4:])
	if err != nil {
		return "method", fmt.Sprintf("%s does not decode: %v", m.Name, err), nil
	}
	if packed, err := m.Inputs.Pack(args...); err != nil || string(packed) != string(data[4:]) {
		return "method", fmt.Sprintf("%s is not encoded canonically", m.Name), nil
	}
	id := common.Hash(args[0].([32]byte))
	r.method, r.id = m.Name, &id

	var status string
	var token, recipient []byte
	var amount string
	err = db.QueryRow(ctx, "SELECT status, token, recipient, amount::text FROM withdrawals WHERE id = $1 AND vault = $2", id.Bytes(), p.Vault.Bytes()).
		Scan(&status, &token, &recipient, &amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "ledger", fmt.Sprintf("the ledger has no withdrawal %s for this vault", id), nil
	}
	if err != nil {
		return "", "", fmt.Errorf("signer: read withdrawal %s: %w", id, err)
	}
	ledgerToken, ledgerTo := common.BytesToAddress(token), common.BytesToAddress(recipient)
	ledgerAmount, _ := new(big.Int).SetString(amount, 10)
	r.token, r.recipient, r.amount = &ledgerToken, &ledgerTo, ledgerAmount
	if status != "requested" && status != "onchain" {
		return "ledger", fmt.Sprintf("withdrawal %s is %s, not open", id, status), nil
	}
	if m.Name == "executeQueued" {
		return "", "", nil
	}
	askedToken, askedTo, askedAmount := args[1].(common.Address), args[2].(common.Address), args[3].(*big.Int)
	if askedToken != ledgerToken || askedTo != ledgerTo || askedAmount.Cmp(ledgerAmount) != 0 {
		r.token, r.recipient, r.amount = &askedToken, &askedTo, askedAmount
		return "ledger", fmt.Sprintf("withdraw pays %s of %s to %s; the ledger holds %s of %s to %s",
			askedAmount, askedToken, askedTo, ledgerAmount, ledgerToken, ledgerTo), nil
	}

	// The velocity limit: each withdrawal signed in the window counts once, this one
	// included, however often it was signed again (a replacement).
	var spent string
	if err := db.QueryRow(ctx, `SELECT coalesce(sum(amount), 0)::text FROM (
			SELECT DISTINCT ON (withdrawal_id) amount FROM signer_log
			WHERE decision = 'signed' AND method = 'withdraw' AND token = $1 AND at > now() - make_interval(secs => $2)
				AND withdrawal_id <> $3
			ORDER BY withdrawal_id, at) s`, ledgerToken.Bytes(), p.Window.Seconds(), id.Bytes()).Scan(&spent); err != nil {
		return "", "", fmt.Errorf("signer: read the window: %w", err)
	}
	total, _ := new(big.Int).SetString(spent, 10)
	total.Add(total, ledgerAmount)
	if total.Cmp(p.Limit) > 0 {
		return "velocity", fmt.Sprintf("%s wei signed for %s in the last %s with this one, at most %s", total, ledgerToken, p.Window, p.Limit), nil
	}
	return "", "", nil
}

func (s *Service) write(ctx context.Context, db pgx.Tx, r record) error {
	tx := r.tx
	var to, token, recipient, id, hash []byte
	if tx.To() != nil {
		to = tx.To().Bytes()
	}
	if r.token != nil {
		token = r.token.Bytes()
	}
	if r.recipient != nil {
		recipient = r.recipient.Bytes()
	}
	if r.id != nil {
		id = r.id.Bytes()
	}
	if r.hash != nil {
		hash = r.hash.Bytes()
	}
	var amount *string
	if r.amount != nil {
		a := r.amount.String()
		amount = &a
	}
	chainID := tx.ChainId()
	if !chainID.IsInt64() {
		chainID = big.NewInt(-1)
	}
	if _, err := db.Exec(ctx, `INSERT INTO signer_log (client, decision, rule, reason, chain_id, sender, nonce, gas, tip_cap, fee_cap,
		destination, method, withdrawal_id, token, recipient, amount, tx_hash)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9::numeric, $10::numeric, $11, NULLIF($12, ''), $13, $14, $15, $16::numeric, $17)`,
		r.client, r.decision, r.rule, r.reason, chainID.Int64(), s.Key.Address().Bytes(), tx.Nonce(), tx.Gas(),
		tx.GasTipCap().String(), tx.GasFeeCap().String(), to, r.method, id, token, recipient, amount, hash); err != nil {
		return fmt.Errorf("signer: record the decision: %w", err)
	}
	return nil
}
