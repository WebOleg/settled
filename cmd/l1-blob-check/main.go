// Command l1-blob-check proves that the local L1 accepts blob transactions: it sends one
// transaction with one blob from a funded devnet account to itself and waits until the
// transaction is included with blob gas used.
//
// The devnet mnemonic is read from SETTLED_DEVNET_MNEMONIC and is never printed.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"

	"github.com/WebOleg/settled/internal/blobtx"
	"github.com/WebOleg/settled/internal/devkeys"
)

const mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "L1 execution JSON-RPC endpoint")
	index := flag.Uint("index", 0, "account index on the devnet mnemonic")
	timeout := flag.Duration("timeout", 90*time.Second, "time allowed for the whole check")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, *rpcURL, uint32(*index)); err != nil {
		fmt.Fprintf(os.Stderr, "l1-blob-check: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("l1-blob-check: ok")
}

func run(ctx context.Context, rpcURL string, index uint32) error {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	key, err := devkeys.FromMnemonic(mnemonic, index)
	if err != nil {
		return fmt.Errorf("derive devnet key: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", rpcURL, err)
	}
	defer client.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read chain id: %w", err)
	}
	balance, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("read balance of %s: %w", from, err)
	}
	if balance.Sign() == 0 {
		return fmt.Errorf("account %s has no balance: is this the devnet started by make l1-up?", from)
	}
	fmt.Printf("account          %s (index %d), chain id %s\n", from, index, chainID)

	payload := fmt.Appendf(nil, "settled l1 blob check %s", time.Now().UTC().Format(time.RFC3339))
	sidecar, err := blobtx.Sidecar(payload)
	if err != nil {
		return err
	}

	tx, err := signedBlobTx(ctx, client, key, chainID, from, sidecar)
	if err != nil {
		return err
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		return fmt.Errorf("send blob transaction: %w", err)
	}
	fmt.Printf("sent             %s, blob hash %s\n", tx.Hash(), tx.BlobHashes()[0])

	receipt, err := waitForReceipt(ctx, client, tx.Hash())
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return fmt.Errorf("transaction %s failed in block %s", tx.Hash(), receipt.BlockNumber)
	}
	if receipt.BlobGasUsed == 0 {
		return fmt.Errorf("transaction %s was included without blob gas", tx.Hash())
	}
	header, err := client.HeaderByNumber(ctx, receipt.BlockNumber)
	if err != nil {
		return fmt.Errorf("read block %s: %w", receipt.BlockNumber, err)
	}
	if header.BlobGasUsed == nil || *header.BlobGasUsed < receipt.BlobGasUsed {
		return fmt.Errorf("block %s does not account for the blob gas of the transaction", receipt.BlockNumber)
	}
	fmt.Printf("included         block %s, blob gas used %d, blob gas price %s wei\n",
		receipt.BlockNumber, receipt.BlobGasUsed, receipt.BlobGasPrice)
	return nil
}

// signedBlobTx builds and signs a zero-value transfer to the sender carrying the blob.
// Fee caps are twice the current base fees, so one fee rise does not stall the check.
func signedBlobTx(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey,
	chainID *big.Int, from common.Address, sidecar *types.BlobTxSidecar) (*types.Transaction, error) {
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, fmt.Errorf("read nonce of %s: %w", from, err)
	}
	tip, err := client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, fmt.Errorf("read gas tip: %w", err)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("read head header: %w", err)
	}
	if head.BaseFee == nil {
		return nil, errors.New("head header has no base fee")
	}
	blobBaseFee, err := client.BlobBaseFee(ctx)
	if err != nil {
		return nil, fmt.Errorf("read blob base fee: %w", err)
	}

	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	blobFeeCap := new(big.Int).Mul(blobBaseFee, big.NewInt(2))
	if blobFeeCap.Sign() == 0 {
		blobFeeCap.SetInt64(1)
	}

	inner := &types.BlobTx{
		ChainID:    uint256.MustFromBig(chainID),
		Nonce:      nonce,
		GasTipCap:  uint256.MustFromBig(tip),
		GasFeeCap:  uint256.MustFromBig(feeCap),
		Gas:        21000,
		To:         from,
		Value:      uint256.NewInt(0),
		BlobFeeCap: uint256.MustFromBig(blobFeeCap),
		BlobHashes: sidecar.BlobHashes(),
		Sidecar:    sidecar,
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), inner)
	if err != nil {
		return nil, fmt.Errorf("sign blob transaction: %w", err)
	}
	return tx, nil
}

func waitForReceipt(ctx context.Context, client *ethclient.Client, hash common.Hash) (*types.Receipt, error) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, hash)
		if err == nil {
			return receipt, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, fmt.Errorf("read receipt of %s: %w", hash, err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("transaction %s not included in time: %w", hash, ctx.Err())
		case <-tick.C:
		}
	}
}
