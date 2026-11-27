// Command bridge-e2e runs the deposit indexer against a real node that can be made to
// reorganise: anvil, which make check-bridge starts with a throwaway PostgreSQL. It
// deploys the vault as on the devnet, behind its proxy, sends real deposits, and steps
// the indexer between moves of the chain:
//
//  1. two deposits are held while their blocks are not final;
//  2. they are credited once the node's finalized block reaches them;
//  3. a held deposit is voided when anvil_reorg drops its block;
//  4. a deposit whose transaction comes back in another block after a reorganisation
//     is voided in the old block, held in the new one and credited once;
//  5. a payment the operator makes without the ledger is posted to suspense once final;
//  6. the ledger agrees with its journal, and what it holds for the vault equals what
//     the vault on chain accounts for at the finalized block, to the wei.
//
// It connects to the database as a login that holds only deposit_indexer.
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
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/txwait"
)

// anvilMnemonic is the public test mnemonic anvil funds its accounts from.
const anvilMnemonic = "test test test test test test test test test test test junk"

// anvilFinality is how many blocks anvil's finalized block trails its head.
const anvilFinality = 64

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "anvil's JSON-RPC")
	outDir := flag.String("out", "contracts/out", "forge build output")
	db := flag.String("db", "postgres://indexer@127.0.0.1:5432/ledger?sslmode=disable", "the ledger database, without the password")
	passwordFile := flag.String("db-password-file", "", "file holding the database password")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir, *db, *passwordFile); err != nil {
		fmt.Fprintf(os.Stderr, "bridge-e2e: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("bridge-e2e: ok")
}

func ok(name, detail string) { fmt.Printf("ok    %-50s %s\n", name, detail) }

func run(ctx context.Context, rpcURL, outDir, db, passwordFile string) error {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	dsn, err := ledger.DSN(db, passwordFile)
	if err != nil {
		return err
	}
	l, err := ledger.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer l.Close()

	keys := make([]common.Address, 3)
	admin, err := devkeys.FromMnemonic(anvilMnemonic, 0)
	if err != nil {
		return err
	}
	for i := range keys {
		k, err := devkeys.FromMnemonic(anvilMnemonic, uint32(i))
		if err != nil {
			return err
		}
		keys[i] = crypto.PubkeyToAddress(k.PublicKey)
	}
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	proxy, err := artifact.Load(outDir, "ERC1967Proxy.sol", "ERC1967Proxy")
	if err != nil {
		return err
	}
	// Past anvil's finality distance first, so the finalized block exists.
	if err := mine(ctx, client, anvilFinality+2); err != nil {
		return err
	}
	code, err := vault.DeployData()
	if err != nil {
		return err
	}
	_, impl, err := txsend.Deploy(ctx, client, admin, code)
	if err != nil {
		return fmt.Errorf("deploy the implementation: %w", err)
	}
	init, err := vault.Call("initialize", keys[0], new(big.Int).SetUint64(300), keys[1], keys[2], uint64(60))
	if err != nil {
		return err
	}
	code, err = proxy.DeployData(impl, init)
	if err != nil {
		return err
	}
	receipt, at, err := txsend.Deploy(ctx, client, admin, code)
	if err != nil {
		return fmt.Errorf("deploy the proxy: %w", err)
	}
	type tokenConfig struct {
		Allowed        bool
		Capacity       *big.Int
		RefillPeriod   uint64
		LargeThreshold *big.Int
	}
	cfg, err := vault.Call("setTokenConfig", deposits.Native, tokenConfig{true, ether(10), 1800, ether(5)})
	if err != nil {
		return err
	}
	if _, err := txsend.Send(ctx, client, admin, at, new(big.Int), cfg); err != nil {
		return fmt.Errorf("configure ETH: %w", err)
	}
	ok("vault deployed behind its proxy", fmt.Sprintf("%s, block %s", at, receipt.BlockNumber))

	account := common.HexToHash("0xe2e")
	x := &deposits.Indexer{Chain: deposits.RPC{Client: client}, Ledger: l, Vault: at, Start: receipt.BlockNumber.Uint64(), MaxRange: 16, MaxBlocks: 300}
	step := func(what string) (deposits.Report, error) {
		rep, err := x.Step(ctx)
		if err != nil {
			return rep, fmt.Errorf("step %s: %w", what, err)
		}
		return rep, nil
	}
	balance := func(what string, pending, posted int64) error {
		a, err := l.Account(ctx, deposits.CustomerAccount(account, "ETH"))
		if err != nil {
			return err
		}
		if a.CreditsPending.Cmp(ether(pending)) != 0 || a.CreditsPosted.Cmp(ether(posted)) != 0 {
			return fmt.Errorf("%s: held %s, credited %s wei; want %d and %d ETH", what, a.CreditsPending, a.CreditsPosted, pending, posted)
		}
		ok(what, fmt.Sprintf("held %d ETH, credited %d ETH", pending, posted))
		return nil
	}

	// 1. Held while not final.
	for _, n := range []int64{1, 2} {
		if _, err := deposit(ctx, client, vault, at, admin, account, n, true); err != nil {
			return err
		}
	}
	if rep, err := step("after two deposits"); err != nil {
		return err
	} else if rep.Seen != 2 || rep.Posted != 0 {
		return fmt.Errorf("after two deposits: %+v, want 2 seen, none posted", rep)
	}
	if err := balance("two deposits held, not final", 3, 0); err != nil {
		return err
	}

	// 2. Credited once final.
	if err := mine(ctx, client, anvilFinality+1); err != nil {
		return err
	}
	if rep, err := step("after finality"); err != nil {
		return err
	} else if rep.Posted != 2 {
		return fmt.Errorf("after finality: %+v, want 2 posted", rep)
	}
	if err := balance("both credited once final", 0, 3); err != nil {
		return err
	}

	// 3. A held deposit whose block is dropped.
	if _, err := deposit(ctx, client, vault, at, admin, account, 3, true); err != nil {
		return err
	}
	if _, err := step("after the third deposit"); err != nil {
		return err
	}
	if err := balance("third deposit held", 3, 3); err != nil {
		return err
	}
	if err := reorg(ctx, client, 2, nil); err != nil {
		return err
	}
	rep, err := step("after the reorganisation")
	if err != nil {
		return err
	}
	if rep.Fork == nil || rep.Voided != 1 {
		return fmt.Errorf("after anvil_reorg: %+v, want a fork and 1 voided", rep)
	}
	ok("anvil_reorg dropped its block: voided", fmt.Sprintf("fork at block %d", *rep.Fork))
	if err := balance("nothing of it credited", 0, 3); err != nil {
		return err
	}

	// 4. The same transaction back in another block.
	raw, err := deposit(ctx, client, vault, at, admin, account, 4, true)
	if err != nil {
		return err
	}
	if _, err := step("after the fourth deposit"); err != nil {
		return err
	}
	if err := reorg(ctx, client, 1, raw); err != nil {
		return err
	}
	rep, err = step("after the transaction moved")
	if err != nil {
		return err
	}
	if rep.Voided != 1 || rep.Seen != 1 {
		return fmt.Errorf("after the transaction moved: %+v, want 1 voided and 1 seen", rep)
	}
	ok("its transaction came back in a new block", "voided in the old one, held in the new one")
	if err := mine(ctx, client, anvilFinality+1); err != nil {
		return err
	}
	if _, err := step("after the final finality"); err != nil {
		return err
	}
	if err := balance("credited once", 0, 7); err != nil {
		return err
	}

	// 5. A payment the ledger never requested.
	operator, err := devkeys.FromMnemonic(anvilMnemonic, 1)
	if err != nil {
		return err
	}
	// Half an ETH: the rate limit starts empty and refills over 1,800 blocks.
	half := new(big.Int).Div(ether(1), big.NewInt(2))
	pay, err := vault.Call("withdraw", crypto.Keccak256Hash([]byte("paid without the ledger")), deposits.Native, keys[2], half)
	if err != nil {
		return err
	}
	if _, err := txsend.Send(ctx, client, operator, at, new(big.Int), pay); err != nil {
		return fmt.Errorf("pay without the ledger: %w", err)
	}
	if err := mine(ctx, client, anvilFinality+1); err != nil {
		return err
	}
	if rep, err := step("after the payment without the ledger"); err != nil {
		return err
	} else if rep.Suspense != 1 {
		return fmt.Errorf("after the payment without the ledger: %+v, want 1 to suspense", rep)
	}
	sus, err := l.Account(ctx, deposits.SuspenseAccount("ETH"))
	if err != nil {
		return err
	}
	if sus.DebitsPosted.Cmp(half) != 0 {
		return fmt.Errorf("suspense holds %s wei, want 0.5 ETH", sus.DebitsPosted)
	}
	ok("a payment without the ledger went to suspense", "0.5 ETH")

	// 6. The books, and the vault on chain.
	problems, err := l.Check(ctx)
	if err != nil {
		return err
	}
	if len(problems) != 0 {
		return fmt.Errorf("the books disagree with the journal: %v", problems)
	}
	ok("the books agree with the journal", "ledger_check: none")
	fin, err := client.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return err
	}
	call, err := vault.Call("accounted", deposits.Native)
	if err != nil {
		return err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &at, Data: call}, fin.Number)
	if err != nil {
		return fmt.Errorf("accounted(ETH): %w", err)
	}
	onChain := new(big.Int).SetBytes(out)
	v, err := l.Account(ctx, "vault:ETH")
	if err != nil {
		return err
	}
	held := new(big.Int).Sub(v.DebitsPosted, v.CreditsPosted)
	if held.Cmp(onChain) != 0 {
		return fmt.Errorf("the ledger holds %s wei for the vault, the vault accounts for %s at block %s", held, onChain, fin.Number)
	}
	ok("ledger equals the vault on chain", fmt.Sprintf("%s wei at finalized block %s", held, fin.Number))
	return nil
}

func ether(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

// deposit sends n ETH for account and returns the signed transaction, so a
// reorganisation can include it again. With wait it waits for the receipt.
func deposit(ctx context.Context, client *ethclient.Client, vault *artifact.Artifact, at common.Address, key *ecdsa.PrivateKey, account common.Hash, n int64, wait bool) ([]byte, error) {
	data, err := vault.Call("deposit", deposits.Native, ether(n), account)
	if err != nil {
		return nil, err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return nil, err
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(chainID), &types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1), GasFeeCap: new(big.Int).Mul(head.BaseFee, big.NewInt(4)),
		Gas: 200_000, To: &at, Value: ether(n), Data: data,
	})
	if err != nil {
		return nil, err
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("deposit %d ETH: %w", n, err)
	}
	if wait {
		r, err := txwait.Receipt(ctx, client, tx.Hash())
		if err != nil {
			return nil, err
		}
		if r.Status != types.ReceiptStatusSuccessful {
			return nil, errors.New("the deposit reverted")
		}
	}
	return tx.MarshalBinary()
}

func mine(ctx context.Context, client *ethclient.Client, n uint64) error {
	if err := client.Client().CallContext(ctx, nil, "anvil_mine", hexutil.Uint64(n)); err != nil {
		return fmt.Errorf("anvil_mine: %w", err)
	}
	return nil
}

// reorg replaces the top depth blocks; raw, when given, goes into the first new block.
func reorg(ctx context.Context, client *ethclient.Client, depth uint64, raw []byte) error {
	pairs := []any{}
	if raw != nil {
		pairs = append(pairs, []any{hexutil.Bytes(raw), 0})
	}
	if err := client.Client().CallContext(ctx, nil, "anvil_reorg", depth, pairs); err != nil {
		return fmt.Errorf("anvil_reorg: %w", err)
	}
	return nil
}
