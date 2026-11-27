// Package sweep moves the whole balance of a retired key to another account, which is
// what a rotation does with a hot wallet's funds: the old key is emptied before it is
// put away. The transfer is signed through the key's own two calls (its address and the
// signature of a hash), so a key in a KMS is swept without leaving it.
//
// What stays behind is only what could not be known before the transfer was mined: the
// fee cap above the fee paid, and on an OP Stack chain the margin kept for the L1 data
// fee. Both are reported.
package sweep

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/txwait"
)

// Key signs for one account.
type Key interface {
	Address() common.Address
	// SignHash returns the 65-byte [R || S || V] signature of hash, V being 0 or 1.
	SignHash(ctx context.Context, hash common.Hash) ([]byte, error)
}

// gasPriceOracle is the OP Stack predeploy that prices a transaction's L1 data fee.
var gasPriceOracle = common.HexToAddress("0x420000000000000000000000000000000000000F")

// Result is what a sweep moved and what was left behind.
type Result struct {
	Moved, Left *big.Int
}

// Sweep sends from's balance to to, less what the transfer costs. A balance that would
// not pay for the transfer is left as it is, and Moved is zero. A key with transactions
// still pending is not swept: its balance is not settled yet.
func Sweep(ctx context.Context, client *ethclient.Client, from Key, to common.Address) (Result, error) {
	account := from.Address()
	nonce, err := client.NonceAt(ctx, account, nil)
	if err != nil {
		return Result{}, err
	}
	pending, err := client.PendingNonceAt(ctx, account)
	if err != nil {
		return Result{}, err
	}
	if pending != nonce {
		return Result{}, fmt.Errorf("sweep: %s has %d transactions pending", account, pending-nonce)
	}
	balance, err := client.BalanceAt(ctx, account, nil)
	if err != nil {
		return Result{}, err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return Result{}, err
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	if head.BaseFee == nil {
		return Result{}, errors.New("sweep: the head has no base fee")
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return Result{}, err
	}
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: account, To: &to, Value: big.NewInt(1)})
	if err != nil {
		return Result{}, err
	}
	tx := &types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip), Gas: gas, To: &to, Value: balance}
	l1, err := l1Fee(ctx, client, tx)
	if err != nil {
		return Result{}, err
	}
	// The L1 data fee is priced when the transfer is mined, from the L1 fees then: twice
	// today's covers a rise of the L1 base fee while it waits.
	cost := new(big.Int).Add(new(big.Int).Mul(new(big.Int).SetUint64(gas), tx.GasFeeCap), new(big.Int).Mul(l1, big.NewInt(2)))
	if balance.Cmp(cost) <= 0 {
		return Result{Moved: new(big.Int), Left: balance}, nil
	}
	tx.Value = new(big.Int).Sub(balance, cost)
	s := types.LatestSignerForChainID(chainID)
	unsigned := types.NewTx(tx)
	sig, err := from.SignHash(ctx, s.Hash(unsigned))
	if err != nil {
		return Result{}, fmt.Errorf("sweep: sign: %w", err)
	}
	signed, err := unsigned.WithSignature(s, sig)
	if err != nil {
		return Result{}, err
	}
	if sender, err := types.Sender(s, signed); err != nil || sender != account {
		return Result{}, fmt.Errorf("sweep: the signature is not %s's (%s, %v)", account, sender, err)
	}
	if err := client.SendTransaction(ctx, signed); err != nil {
		return Result{}, fmt.Errorf("sweep: send: %w", err)
	}
	receipt, err := txwait.Receipt(ctx, client, signed.Hash())
	if err != nil {
		return Result{}, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return Result{}, fmt.Errorf("sweep: %s reverted", signed.Hash())
	}
	left, err := client.BalanceAt(ctx, account, receipt.BlockNumber)
	if err != nil {
		return Result{}, err
	}
	return Result{Moved: tx.Value, Left: left}, nil
}

// l1Fee is the L1 data fee the transfer would pay on an OP Stack chain, from the gas
// price oracle; zero on a chain without one.
func l1Fee(ctx context.Context, client *ethclient.Client, tx *types.DynamicFeeTx) (*big.Int, error) {
	code, err := client.CodeAt(ctx, gasPriceOracle, nil)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return new(big.Int), nil
	}
	raw, err := types.NewTx(tx).MarshalBinary()
	if err != nil {
		return nil, err
	}
	// getL1Fee(bytes): the selector, the offset of the bytes, their length, the bytes.
	data := append(crypto.Keccak256([]byte("getL1Fee(bytes)"))[:4], common.LeftPadBytes(big.NewInt(32).Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(big.NewInt(int64(len(raw))).Bytes(), 32)...)
	data = append(data, common.RightPadBytes(raw, (len(raw)+31)/32*32)...)
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &gasPriceOracle, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("sweep: price the L1 data fee: %w", err)
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("sweep: getL1Fee answered %d bytes", len(out))
	}
	return new(big.Int).SetBytes(out), nil
}
