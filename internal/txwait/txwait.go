// Package txwait waits for transactions to be included. It is the one way every command
// in this repository waits for a receipt.
package txwait

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ReceiptReader is the part of ethclient.Client that Receipt needs.
type ReceiptReader interface {
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// PollInterval is how often Receipt asks for the receipt. The local L1 has 6 s slots.
const PollInterval = 2 * time.Second

// Receipt polls until the transaction has a receipt or ctx ends. A missing receipt is
// normal while the transaction is pending; any other error stops the wait.
func Receipt(ctx context.Context, client ReceiptReader, hash common.Hash) (*types.Receipt, error) {
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, hash)
		if err == nil {
			return receipt, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, fmt.Errorf("txwait: read receipt of %s: %w", hash, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("txwait: %s not included in time: %w", hash, ctx.Err())
		case <-tick.C:
		}
	}
}
