// Package funding tops devnet accounts up to a target balance. Topping up, rather than
// sending a fixed amount, makes a rerun safe: an account that already holds its target
// receives nothing.
package funding

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/txwait"
)

// Target is an account and the balance it must hold, in wei.
type Target struct {
	Name    string
	Address common.Address
	Balance *big.Int
}

// Result reports what TopUp did for one target.
type Result struct {
	Target Target
	Before *big.Int
	Sent   *big.Int
	TxHash common.Hash // zero when nothing was sent
}

// Ether returns n ETH in wei.
func Ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000_000_000_000_000))
}

// FormatEther renders wei as ETH with four decimals, truncated, using integer
// arithmetic only. It is for display; amounts are never stored as text or floats.
func FormatEther(wei *big.Int) string {
	if wei == nil {
		return "0.0000"
	}
	neg := wei.Sign() < 0
	abs := new(big.Int).Abs(wei)
	whole, rest := new(big.Int).QuoRem(abs, Ether(1), new(big.Int))
	frac := rest.Div(rest, big.NewInt(100_000_000_000_000)) // 1e14 wei = 0.0001 ETH
	s := fmt.Sprintf("%s.%04d", whole, frac.Int64())
	if neg {
		s = "-" + s
	}
	return s
}

// Shortfall returns how much must be sent for balance to reach target. It is zero when
// the balance already reaches the target. Negative inputs are a programming error.
func Shortfall(balance, target *big.Int) (*big.Int, error) {
	if balance == nil || target == nil || balance.Sign() < 0 || target.Sign() < 0 {
		return nil, errors.New("funding: balance and target must be non-negative")
	}
	if balance.Cmp(target) >= 0 {
		return new(big.Int), nil
	}
	return new(big.Int).Sub(target, balance), nil
}

// TopUp sends each target its shortfall from key and waits until every transfer is
// included and successful. Transfers are sent with consecutive nonces before waiting,
// so they land in as few blocks as possible.
func TopUp(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, targets []Target) ([]Result, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("funding: read chain id: %w", err)
	}
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("funding: read nonce of %s: %w", from, err)
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("funding: read gas tip: %w", err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("funding: read head header: %w", err)
	}
	if head.BaseFee == nil {
		return nil, errors.New("funding: head header has no base fee")
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	signer := types.LatestSignerForChainID(chainID)

	results := make([]Result, 0, len(targets))
	for _, t := range targets {
		before, err := client.BalanceAt(ctx, t.Address, nil)
		if err != nil {
			return nil, fmt.Errorf("funding: read balance of %s: %w", t.Name, err)
		}
		amount, err := Shortfall(before, t.Balance)
		if err != nil {
			return nil, fmt.Errorf("funding: %s: %w", t.Name, err)
		}
		r := Result{Target: t, Before: before, Sent: amount}
		if amount.Sign() > 0 {
			tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
				ChainID:   chainID,
				Nonce:     nonce,
				GasTipCap: tip,
				GasFeeCap: feeCap,
				Gas:       21000,
				To:        &t.Address,
				Value:     amount,
			})
			if err != nil {
				return nil, fmt.Errorf("funding: sign transfer to %s: %w", t.Name, err)
			}
			if err := client.SendTransaction(ctx, tx); err != nil {
				return nil, fmt.Errorf("funding: send transfer to %s: %w", t.Name, err)
			}
			nonce++
			r.TxHash = tx.Hash()
		}
		results = append(results, r)
	}

	for _, r := range results {
		if r.TxHash == (common.Hash{}) {
			continue
		}
		receipt, err := txwait.Receipt(ctx, client, r.TxHash)
		if err != nil {
			return nil, fmt.Errorf("funding: transfer to %s: %w", r.Target.Name, err)
		}
		if receipt.Status != types.ReceiptStatusSuccessful {
			return nil, fmt.Errorf("funding: transfer to %s failed in block %s", r.Target.Name, receipt.BlockNumber)
		}
	}
	return results, nil
}
