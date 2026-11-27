// Package txsend sends one contract call or transfer and waits for it to succeed, and
// works out what it cost. Every command that moves funds goes through it, so the fee
// rules and the success check live in one place.
package txsend

import (
	"context"
	"crypto/ecdsa"
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

// ErrReverted is returned for a transaction that was included but failed.
var ErrReverted = errors.New("txsend: transaction reverted")

// GasMarginPercent is added to the gas estimate, which is exact for the state it was
// made on and may be short by the time the transaction is included.
const GasMarginPercent = 20

// Send signs a dynamic-fee transaction from key to `to` with value and data, sends it,
// and waits for a successful receipt.
func Send(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, to common.Address, value *big.Int, data []byte) (*types.Receipt, error) {
	return send(ctx, client, key, &to, value, data)
}

// ErrNoContract is returned when a creation succeeds but leaves no code at the address.
var ErrNoContract = errors.New("txsend: no code at the created address")

// Deploy signs and sends a contract creation (creation code followed by the constructor
// arguments), waits for a successful receipt, and returns it with the new address,
// checked to hold code.
func Deploy(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, data []byte) (*types.Receipt, common.Address, error) {
	receipt, err := send(ctx, client, key, nil, new(big.Int), data)
	if err != nil {
		return receipt, common.Address{}, err
	}
	code, err := client.CodeAt(ctx, receipt.ContractAddress, receipt.BlockNumber)
	if err != nil {
		return receipt, common.Address{}, fmt.Errorf("txsend: read code at %s: %w", receipt.ContractAddress, err)
	}
	if len(code) == 0 {
		return receipt, common.Address{}, fmt.Errorf("%w %s", ErrNoContract, receipt.ContractAddress)
	}
	return receipt, receipt.ContractAddress, nil
}

func send(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, to *common.Address, value *big.Int, data []byte) (*types.Receipt, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("txsend: read chain id: %w", err)
	}
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("txsend: read nonce of %s: %w", from, err)
	}
	gas, err := client.EstimateGas(ctx, ethereum.CallMsg{From: from, To: to, Value: value, Data: data})
	if err != nil {
		return nil, fmt.Errorf("txsend: estimate gas: %w", err)
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("txsend: read gas tip: %w", err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("txsend: read head header: %w", err)
	}
	if head.BaseFee == nil {
		return nil, errors.New("txsend: head header has no base fee")
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip),
		Gas:       WithMargin(gas),
		To:        to,
		Value:     value,
		Data:      data,
	})
	if err != nil {
		return nil, fmt.Errorf("txsend: sign: %w", err)
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("txsend: send: %w", err)
	}
	receipt, err := txwait.Receipt(ctx, client, tx.Hash())
	if err != nil {
		return nil, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return receipt, fmt.Errorf("%w: %s in block %s", ErrReverted, tx.Hash(), receipt.BlockNumber)
	}
	return receipt, nil
}

// WithMargin returns gas plus GasMarginPercent.
func WithMargin(gas uint64) uint64 {
	return gas + gas*GasMarginPercent/100
}

// ExecutionCost is what the sender paid for execution: gas used times the effective gas
// price. On an OP Stack L2 the sender also pays an L1 data fee, which is not part of an
// Ethereum receipt and has to be read separately.
func ExecutionCost(r *types.Receipt) *big.Int {
	if r.EffectiveGasPrice == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(r.GasUsed), r.EffectiveGasPrice)
}
