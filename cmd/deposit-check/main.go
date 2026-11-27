// Command deposit-check sends one real deposit to the vault on the devnet and follows
// it through the ledger the deposit indexer keeps, timing each step:
//
//  1. the devnet user deposits 0.1 ETH for a new exchange account, through the
//     sequencer;
//  2. the deposit is held in the ledger (pending: shown, not spendable) while the
//     verifier's finalized block is still below it;
//  3. it is credited once, and only once, the verifier's finalized block reaches it;
//  4. the books agree with the journal.
//
// It reads the ledger as the auditor, who can only read. The run takes as long as
// finality: about a quarter of an hour on the devnet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/vaultconf"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// userIndex is the devnet user, who holds ETH on the L2.
	userIndex = 2
)

func main() {
	seqURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2, to send the deposit")
	verURL := flag.String("verifier", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2, whose finalized block the indexer follows")
	outDir := flag.String("out", "contracts/out", "forge build output")
	db := flag.String("db", "postgres://auditor@127.0.0.1:15432/ledger?sslmode=disable", "the ledger database, without the password")
	passwordFile := flag.String("db-password-file", ".devnet/bridge/auditor.txt", "file holding the database password")
	timeout := flag.Duration("timeout", 40*time.Minute, "how long to wait for the deposit to be credited")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+2*time.Minute)
	defer cancel()
	if err := run(ctx, *seqURL, *verURL, *outDir, *db, *passwordFile, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "deposit-check: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("deposit-check: ok")
}

func ok(name, detail string) { fmt.Printf("ok    %-46s %s\n", name, detail) }

func run(ctx context.Context, seqURL, verURL, outDir, db, passwordFile string, timeout time.Duration) error {
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return fmt.Errorf("%w (run make vault-deploy)", err)
	}
	seq, err := ethclient.DialContext(ctx, seqURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", seqURL, err)
	}
	defer seq.Close()
	ver, err := ethclient.DialContext(ctx, verURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", verURL, err)
	}
	defer ver.Close()
	genesis, err := ver.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return errors.New("the deployment record belongs to another L2")
	}
	dsn, err := ledger.DSN(db, passwordFile)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect as the auditor: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	user, err := devkeys.FromMnemonic(mnemonic, userIndex)
	if err != nil {
		return fmt.Errorf("derive the devnet user key: %w", err)
	}
	userAddr := crypto.PubkeyToAddress(user.PublicKey)
	amount := new(big.Int).Div(funding.Ether(1), big.NewInt(10)) // 0.1 ETH
	balance, err := seq.BalanceAt(ctx, userAddr, nil)
	if err != nil {
		return err
	}
	if need := new(big.Int).Mul(amount, big.NewInt(2)); balance.Cmp(need) < 0 {
		return fmt.Errorf("the devnet user %s holds %s ETH on the L2, needs %s (make l2-deposit brings ETH from the L1)",
			userAddr, funding.FormatEther(balance), funding.FormatEther(need))
	}

	head, err := seq.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	account := crypto.Keccak256Hash([]byte("settled: deposit-check " + head.Number.String()))
	name := deposits.CustomerAccount(account, "ETH")
	data, err := vault.Call("deposit", deposits.Native, amount, account)
	if err != nil {
		return err
	}
	receipt, err := txsend.Send(ctx, seq, user, d.Vault, amount, data)
	if err != nil {
		return fmt.Errorf("deposit: %w", err)
	}
	sent := time.Now()
	block := receipt.BlockNumber.Uint64()
	ok("deposited 0.1 ETH for a new account", fmt.Sprintf("block %d, account %s", block, account.TerminalString()))

	// read finds the deposit's row by its block and transaction, and the account.
	type state struct {
		status         string
		pending, owned *big.Int
	}
	read := func() (state, error) {
		var s state
		err := conn.QueryRow(ctx, "SELECT status FROM deposits WHERE vault = $1 AND block_hash = $2 AND tx_hash = $3",
			d.Vault.Bytes(), receipt.BlockHash.Bytes(), receipt.TxHash.Bytes()).Scan(&s.status)
		if errors.Is(err, pgx.ErrNoRows) {
			return s, nil
		}
		if err != nil {
			return s, err
		}
		var p, o string
		if err := conn.QueryRow(ctx, "SELECT credits_pending::text, (credits_posted - debits_posted)::text FROM accounts WHERE name = $1",
			name).Scan(&p, &o); err != nil {
			return s, err
		}
		s.pending, _ = new(big.Int).SetString(p, 10)
		s.owned, _ = new(big.Int).SetString(o, 10)
		return s, nil
	}
	finalized := func() (uint64, error) {
		h, err := ver.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
		if err != nil {
			return 0, err
		}
		return h.Number.Uint64(), nil
	}

	deadline := time.Now().Add(timeout)
	held := false
	lastReport := time.Now()
	for {
		s, err := read()
		if err != nil {
			return fmt.Errorf("read the ledger: %w", err)
		}
		fin, err := finalized()
		if err != nil {
			return fmt.Errorf("read the verifier's finalized block: %w", err)
		}
		switch s.status {
		case "":
		case "pending":
			if fin >= block && !held {
				// Seen first when already final: it will be posted at the next step.
				break
			}
			if s.pending.Cmp(amount) != 0 || s.owned.Sign() != 0 {
				return fmt.Errorf("held: the account has %s wei pending and %s credited; want %s and 0", s.pending, s.owned, amount)
			}
			if !held {
				held = true
				ok("held in the ledger, not spendable", fmt.Sprintf("after %s; finalized is at %d, %d blocks below", since(sent), fin, block-fin))
			}
		case "posted":
			if !held {
				return errors.New("the deposit was credited without first being held: the indexer saw it only when already final")
			}
			if fin < block {
				return fmt.Errorf("credited while the verifier's finalized block %d is still below the deposit's block %d", fin, block)
			}
			if s.owned.Cmp(amount) != 0 || s.pending.Sign() != 0 {
				return fmt.Errorf("credited: the account has %s wei credited and %s pending; want %s and 0", s.owned, s.pending, amount)
			}
			ok("credited once final", fmt.Sprintf("after %s; finalized is at %d", since(sent), fin))
			rows, err := conn.Query(ctx, "SELECT problem FROM ledger_check()")
			if err != nil {
				return fmt.Errorf("ledger_check: %w", err)
			}
			problems, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return fmt.Errorf("ledger_check: %w", err)
			}
			if len(problems) != 0 {
				return fmt.Errorf("the books disagree with the journal: %v", problems)
			}
			ok("the books agree with the journal", "ledger_check: none")
			return nil
		default:
			return fmt.Errorf("the deposit is %s", s.status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not credited after %s (status %q, finalized %d, deposit block %d)", timeout, s.status, fin, block)
		}
		if time.Since(lastReport) >= time.Minute && held {
			fmt.Printf("wait  %-46s finalized %d, %d blocks to go\n", "for finality", fin, int64(block)-int64(fin))
			lastReport = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }
