// Command l2-fund tops up the L2 operator accounts that pay gas on the L1: the deployer,
// the batcher, the proposer and the challenger. The funds come from the first account of
// the devnet mnemonic, read from SETTLED_DEVNET_MNEMONIC and never printed.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
)

const mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"

// Target balances on the L1. The deployer pays for every contract of the deployment;
// the others pay for batches, output roots and dispute bonds over a long devnet run.
var targets = []struct {
	role  rolekeys.Role
	ether int64
}{
	{rolekeys.Deployer, 100},
	{rolekeys.Batcher, 20},
	{rolekeys.Proposer, 20},
	{rolekeys.Challenger, 20},
}

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "L1 execution JSON-RPC endpoint")
	dir := flag.String("dir", ".devnet/l2/keys", "directory with the role keys (make l2-keys)")
	timeout := flag.Duration("timeout", 90*time.Second, "time allowed for the whole top-up")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := run(ctx, *rpcURL, *dir); err != nil {
		fmt.Fprintf(os.Stderr, "l2-fund: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("l2-fund: ok")
}

func run(ctx context.Context, rpcURL, dir string) error {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	funder, err := devkeys.FromMnemonic(mnemonic, 0)
	if err != nil {
		return fmt.Errorf("derive funder key: %w", err)
	}

	list := make([]funding.Target, 0, len(targets))
	for _, t := range targets {
		key, err := rolekeys.Load(dir, t.role)
		if err != nil {
			return fmt.Errorf("load %s key (run make l2-keys first): %w", t.role, err)
		}
		list = append(list, funding.Target{
			Name:    string(t.role),
			Address: crypto.PubkeyToAddress(key.PublicKey),
			Balance: funding.Ether(t.ether),
		})
	}

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", rpcURL, err)
	}
	defer client.Close()

	fmt.Printf("funder           %s\n", crypto.PubkeyToAddress(funder.PublicKey))
	results, err := funding.TopUp(ctx, client, funder, list)
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Sent.Sign() == 0 {
			fmt.Printf("%-16s %s  already holds %s ETH\n", r.Target.Name, r.Target.Address, funding.FormatEther(r.Before))
			continue
		}
		fmt.Printf("%-16s %s  %s -> %s ETH  tx %s\n", r.Target.Name, r.Target.Address,
			funding.FormatEther(r.Before), funding.FormatEther(new(big.Int).Add(r.Before, r.Sent)), r.TxHash)
	}
	return nil
}
