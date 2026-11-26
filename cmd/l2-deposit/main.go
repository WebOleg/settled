// Command l2-deposit moves ETH from the L1 to the L2 through OptimismPortal and accounts
// for every wei of it. The sender is the devnet user, an account of the public devnet
// mnemonic (read from SETTLED_DEVNET_MNEMONIC, never printed), and the ETH goes to the
// same address on the L2. It checks, each exactly:
//
//   - on the L1, the ETHLockbox and the portal together hold the deposited amount more
//     after the deposit block than before it, and the user paid the amount plus the gas
//     of the deposit;
//   - on the L2, the user's balance grows by the amount in one block, through a deposit
//     transaction (type 0x7e) that mints exactly that amount;
//   - that block becomes safe on the verifier, which derives it from the L1. A deposit is
//     counted as received only then.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/portal"
	"github.com/WebOleg/settled/internal/txsend"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// UserIndex is the devnet user on the devnet mnemonic. Index 0 funds the operators
	// and sends the blob check, so the user has its own account and its own nonces.
	UserIndex = 2
	// DepositGasLimit is the L2 gas the deposit buys for its execution, a plain transfer.
	DepositGasLimit = 100_000
	depositTxType   = "0x7e"
)

func main() {
	l1URL := flag.String("l1", "http://127.0.0.1:8545", "L1 execution JSON-RPC")
	l2URL := flag.String("l2", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2")
	nodeURL := flag.String("node", "http://127.0.0.1:29545", "verifier op-node RPC")
	dir := flag.String("deployer-dir", ".devnet/l2/deployer", "files written by make l2-deploy")
	ether := flag.Int64("ether", 1, "amount to deposit, in whole ETH")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := run(ctx, *l1URL, *l2URL, *nodeURL, *dir, funding.Ether(*ether)); err != nil {
		fmt.Fprintf(os.Stderr, "l2-deposit: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("l2-deposit: ok")
}

func run(ctx context.Context, l1URL, l2URL, nodeURL, dir string, amount *big.Int) error {
	if amount.Sign() <= 0 {
		return errors.New("amount must be positive")
	}
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	key, err := devkeys.FromMnemonic(mnemonic, UserIndex)
	if err != nil {
		return fmt.Errorf("derive the devnet user key: %w", err)
	}
	user := crypto.PubkeyToAddress(key.PublicKey)

	contracts := map[string]common.Address{}
	if err := readJSON(filepath.Join(dir, "l1-addresses.json"), &contracts); err != nil {
		return err
	}
	portalAddr, lockbox := contracts["OptimismPortalProxy"], contracts["EthLockboxProxy"]
	if portalAddr == (common.Address{}) || lockbox == (common.Address{}) {
		return errors.New("l1-addresses.json lacks OptimismPortalProxy or EthLockboxProxy")
	}

	l1, err := ethclient.DialContext(ctx, l1URL)
	if err != nil {
		return fmt.Errorf("connect to the L1: %w", err)
	}
	defer l1.Close()
	l2raw, err := rpc.DialContext(ctx, l2URL)
	if err != nil {
		return fmt.Errorf("connect to the L2: %w", err)
	}
	defer l2raw.Close()
	l2 := ethclient.NewClient(l2raw)
	node, err := rpc.DialContext(ctx, nodeURL)
	if err != nil {
		return fmt.Errorf("connect to op-node: %w", err)
	}
	defer node.Close()

	fmt.Printf("user             %s (devnet mnemonic index %d)\n", user, UserIndex)
	fmt.Printf("amount           %s ETH (%s wei)\n", funding.FormatEther(amount), amount)
	l1Balance, err := l1.BalanceAt(ctx, user, nil)
	if err != nil {
		return err
	}
	if l1Balance.Cmp(new(big.Int).Mul(amount, big.NewInt(2))) < 0 {
		return fmt.Errorf("the user holds %s wei on the L1, too little for %s wei", l1Balance, amount)
	}
	l2Start, err := l2.BlockNumber(ctx)
	if err != nil {
		return err
	}
	l2Before, err := l2.BalanceAt(ctx, user, new(big.Int).SetUint64(l2Start))
	if err != nil {
		return err
	}

	data, err := portal.Deposit(user, amount, DepositGasLimit)
	if err != nil {
		return err
	}
	receipt, err := txsend.Send(ctx, l1, key, portalAddr, amount, data)
	if err != nil {
		return fmt.Errorf("deposit: %w", err)
	}
	block := receipt.BlockNumber
	prev := new(big.Int).Sub(block, big.NewInt(1))
	fmt.Printf("L1 deposit       tx %s in L1 block %s\n", receipt.TxHash, block)

	// L1 side, read at the block before and the block of the deposit, so nothing else
	// that happens on the L1 meanwhile can enter the numbers.
	// The portal keeps deposited ETH in the ETHLockbox when the lockbox is in use, and
	// itself otherwise; together they must hold exactly the amount more.
	held := map[string]*big.Int{}
	total := new(big.Int)
	for name, addr := range map[string]common.Address{"ETHLockbox": lockbox, "OptimismPortal": portalAddr} {
		before, err := l1.BalanceAt(ctx, addr, prev)
		if err != nil {
			return err
		}
		after, err := l1.BalanceAt(ctx, addr, block)
		if err != nil {
			return err
		}
		held[name] = new(big.Int).Sub(after, before)
		total.Add(total, held[name])
	}
	if total.Cmp(amount) != 0 {
		return fmt.Errorf("the portal and the lockbox grew by %s wei together, want %s", total, amount)
	}
	fmt.Printf("L1 custody       +%s wei: ETHLockbox +%s, OptimismPortal +%s\n", amount, held["ETHLockbox"], held["OptimismPortal"])

	userBefore, err := l1.BalanceAt(ctx, user, prev)
	if err != nil {
		return err
	}
	userAfter, err := l1.BalanceAt(ctx, user, block)
	if err != nil {
		return err
	}
	cost := txsend.ExecutionCost(receipt)
	paid := new(big.Int).Sub(userBefore, userAfter)
	if want := new(big.Int).Add(amount, cost); paid.Cmp(want) != 0 {
		return fmt.Errorf("the user paid %s wei on the L1, want amount + gas = %s", paid, want)
	}
	fmt.Printf("L1 user          paid amount + gas: %s + %s wei\n", amount, cost)

	// L2 side: wait until the balance has grown, then find the exact block.
	target := new(big.Int).Add(l2Before, amount)
	credited, err := waitForBalance(ctx, l2, user, target, l2Start)
	if err != nil {
		return err
	}
	if err := checkDepositTx(ctx, l2raw, credited, user, amount); err != nil {
		return err
	}
	fmt.Printf("L2 credit        +%s wei in L2 block %d, by a deposit transaction minting exactly that\n", amount, credited)

	safe, err := waitForSafe(ctx, node, credited)
	if err != nil {
		return err
	}
	fmt.Printf("L2 safe          block %d is safe on the verifier (safe head %d): derived from the L1\n", credited, safe)
	return nil
}

// waitForBalance waits until addr holds want on the L2 and returns the first block in
// which it does, found by bisection between from and the head where it was seen.
func waitForBalance(ctx context.Context, l2 *ethclient.Client, addr common.Address, want *big.Int, from uint64) (uint64, error) {
	var head uint64
	for {
		n, err := l2.BlockNumber(ctx)
		if err != nil {
			return 0, err
		}
		b, err := l2.BalanceAt(ctx, addr, new(big.Int).SetUint64(n))
		if err != nil {
			return 0, err
		}
		if b.Cmp(want) == 0 {
			head = n
			break
		}
		if b.Cmp(want) > 0 {
			return 0, fmt.Errorf("L2 balance %s passed the expected %s; something else paid the user", b, want)
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("deposit not credited on the L2: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	lo, hi := from, head // balance < want at lo, == want at hi
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		b, err := l2.BalanceAt(ctx, addr, new(big.Int).SetUint64(mid))
		if err != nil {
			return 0, err
		}
		if b.Cmp(want) == 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi, nil
}

// checkDepositTx finds, in the L2 block, the deposit transaction to user and checks that
// it minted amount.
func checkDepositTx(ctx context.Context, l2 *rpc.Client, number uint64, user common.Address, amount *big.Int) error {
	var block struct {
		Transactions []struct {
			Type string          `json:"type"`
			To   *common.Address `json:"to"`
			Mint *hexutil.Big    `json:"mint"`
		} `json:"transactions"`
	}
	if err := l2.CallContext(ctx, &block, "eth_getBlockByNumber", hexutil.Uint64(number), true); err != nil {
		return fmt.Errorf("read L2 block %d: %w", number, err)
	}
	for _, tx := range block.Transactions {
		if !strings.EqualFold(tx.Type, depositTxType) || tx.To == nil || *tx.To != user {
			continue
		}
		if tx.Mint == nil || tx.Mint.ToInt().Cmp(amount) != 0 {
			return fmt.Errorf("the deposit transaction in L2 block %d mints %v, want %s", number, tx.Mint, amount)
		}
		return nil
	}
	return fmt.Errorf("L2 block %d has no deposit transaction to %s", number, user)
}

func waitForSafe(ctx context.Context, node *rpc.Client, number uint64) (uint64, error) {
	for {
		var status struct {
			SafeL2 struct {
				Number uint64 `json:"number"`
			} `json:"safe_l2"`
		}
		if err := node.CallContext(ctx, &status, "optimism_syncStatus"); err != nil {
			return 0, fmt.Errorf("read op-node sync status: %w", err)
		}
		if status.SafeL2.Number >= number {
			return status.SafeL2.Number, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("L2 block %d not safe in time: %w", number, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
