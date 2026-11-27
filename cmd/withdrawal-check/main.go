// Command withdrawal-check sends one real withdrawal through the devnet's bridge and
// follows it end to end, timing each step:
//
//  1. the devnet user deposits 0.1 ETH for a new exchange account, and the deposit
//     indexer credits it once final;
//  2. the account asks to withdraw 0.04 ETH to a new address: the amount is held at
//     once, before anything is on chain;
//  3. the withdrawal payer, running as a service, pays it from the vault: the vault
//     records the id as paid and the address holds 0.04 ETH;
//  4. once the payment is final, the indexer settles the withdrawal: the hold is
//     posted, the account owns 0.06 ETH, and not before;
//  5. the books agree with the journal.
//
// It records the request as the requester, who can hold a balance and record a
// withdrawal and nothing else, and reads the ledger as the auditor, who can only read.
// The run takes two finalities: about half an hour on the devnet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
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
	"github.com/WebOleg/settled/internal/withdrawals"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// userIndex is the devnet user, who holds ETH on the L2.
	userIndex = 2
)

func main() {
	seqURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	verURL := flag.String("verifier", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2, whose finalized block the indexer follows")
	outDir := flag.String("out", "contracts/out", "forge build output")
	requesterDB := flag.String("requester-db", "postgres://requester@127.0.0.1:15432/ledger?sslmode=disable", "the ledger as the requester, without the password")
	requesterPassword := flag.String("requester-db-password-file", ".devnet/bridge/requester.txt", "file holding the requester's password")
	auditorDB := flag.String("db", "postgres://auditor@127.0.0.1:15432/ledger?sslmode=disable", "the ledger as the auditor, without the password")
	auditorPassword := flag.String("db-password-file", ".devnet/bridge/auditor.txt", "file holding the auditor's password")
	timeout := flag.Duration("timeout", 60*time.Minute, "how long to wait for the whole run")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := &check{ctx: ctx, start: time.Now()}
	if err := c.run(*seqURL, *verURL, *outDir, *requesterDB, *requesterPassword, *auditorDB, *auditorPassword); err != nil {
		fmt.Fprintf(os.Stderr, "withdrawal-check: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("withdrawal-check: ok")
}

func ok(name, detail string) { fmt.Printf("ok    %-46s %s\n", name, detail) }

type check struct {
	ctx      context.Context
	start    time.Time
	seq, ver *ethclient.Client
	auditor  *pgx.Conn
	account  string
}

func since(t time.Time) string { return time.Since(t).Round(time.Second).String() }

// until calls f every 2 s until it returns true, reporting every minute.
func (c *check) until(what string, f func() (bool, string, error)) error {
	last := time.Now()
	for {
		done, status, err := f()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Since(last) >= time.Minute {
			fmt.Printf("wait  %-46s %s\n", what, status)
			last = time.Now()
		}
		select {
		case <-c.ctx.Done():
			return fmt.Errorf("%s: %w (%s)", what, c.ctx.Err(), status)
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *check) finalized() (uint64, error) {
	h, err := c.ver.HeaderByNumber(c.ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return 0, fmt.Errorf("the verifier's finalized block: %w", err)
	}
	return h.Number.Uint64(), nil
}

// balances reads the account's held and owned wei.
func (c *check) balances() (held, owned *big.Int, err error) {
	var h, o string
	err = c.auditor.QueryRow(c.ctx, "SELECT debits_pending::text, (credits_posted - debits_posted)::text FROM accounts WHERE name = $1", c.account).Scan(&h, &o)
	if errors.Is(err, pgx.ErrNoRows) {
		return new(big.Int), new(big.Int), nil
	}
	if err != nil {
		return nil, nil, err
	}
	held, _ = new(big.Int).SetString(h, 10)
	owned, _ = new(big.Int).SetString(o, 10)
	return held, owned, nil
}

func (c *check) run(seqURL, verURL, outDir, requesterDB, requesterPassword, auditorDB, auditorPassword string) error {
	ctx := c.ctx
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return fmt.Errorf("%w (run make vault-deploy)", err)
	}
	if c.seq, err = ethclient.DialContext(ctx, seqURL); err != nil {
		return fmt.Errorf("dial %s: %w", seqURL, err)
	}
	defer c.seq.Close()
	if c.ver, err = ethclient.DialContext(ctx, verURL); err != nil {
		return fmt.Errorf("dial %s: %w", verURL, err)
	}
	defer c.ver.Close()
	genesis, err := c.ver.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return errors.New("the deployment record belongs to another L2")
	}
	dsn, err := ledger.DSN(auditorDB, auditorPassword)
	if err != nil {
		return err
	}
	if c.auditor, err = pgx.Connect(ctx, dsn); err != nil {
		return fmt.Errorf("connect as the auditor: %w", err)
	}
	defer c.auditor.Close(context.WithoutCancel(ctx))
	dsn, err = ledger.DSN(requesterDB, requesterPassword)
	if err != nil {
		return err
	}
	requester, err := ledger.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect as the requester: %w", err)
	}
	defer requester.Close()
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
	deposit := new(big.Int).Div(funding.Ether(1), big.NewInt(10)) // 0.1 ETH
	amount := new(big.Int).Div(funding.Ether(4), big.NewInt(100)) // 0.04 ETH
	left := new(big.Int).Sub(deposit, amount)                     // 0.06 ETH
	balance, err := c.seq.BalanceAt(ctx, userAddr, nil)
	if err != nil {
		return err
	}
	if need := new(big.Int).Mul(deposit, big.NewInt(2)); balance.Cmp(need) < 0 {
		return fmt.Errorf("the devnet user %s holds %s ETH on the L2, needs %s (make l2-deposit brings ETH from the L1)",
			userAddr, funding.FormatEther(balance), funding.FormatEther(need))
	}

	// 1. A deposit, credited once final.
	head, err := c.seq.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	tag := head.Number.String()
	account := crypto.Keccak256Hash([]byte("settled: withdrawal-check " + tag))
	c.account = deposits.CustomerAccount(account, "ETH")
	data, err := vault.Call("deposit", deposits.Native, deposit, account)
	if err != nil {
		return err
	}
	receipt, err := txsend.Send(ctx, c.seq, user, d.Vault, deposit, data)
	if err != nil {
		return fmt.Errorf("deposit: %w", err)
	}
	sent := time.Now()
	ok("deposited 0.1 ETH for a new account", fmt.Sprintf("block %s, account %s", receipt.BlockNumber, account.TerminalString()))
	if err := c.until("for the deposit to be final", func() (bool, string, error) {
		_, owned, err := c.balances()
		if err != nil {
			return false, "", err
		}
		fin, err := c.finalized()
		if err != nil {
			return false, "", err
		}
		return owned.Cmp(deposit) == 0, fmt.Sprintf("finalized %d, %d blocks to go", fin, int64(receipt.BlockNumber.Uint64())-int64(fin)), nil
	}); err != nil {
		return err
	}
	ok("credited once final", fmt.Sprintf("after %s", since(sent)))

	// 2. The request: held at once.
	to := common.BytesToAddress(crypto.Keccak256([]byte("settled: withdrawal-check recipient " + tag))[12:])
	w, err := withdrawals.Submit(ctx, requester, d.Vault, withdrawals.Request{Key: "withdrawal-check " + tag, Account: account, Token: deposits.Native, Recipient: to, Amount: amount})
	if err != nil {
		return fmt.Errorf("request the withdrawal: %w", err)
	}
	requested := time.Now()
	held, owned, err := c.balances()
	if err != nil {
		return err
	}
	if held.Cmp(amount) != 0 || owned.Cmp(deposit) != 0 {
		return fmt.Errorf("after the request: held %s, owned %s wei; want %s held of %s", held, owned, amount, deposit)
	}
	ok("requested 0.04 ETH: held at once", fmt.Sprintf("id %s, to %s", w.ID.TerminalString(), to))

	// 3. Paid by the payer service.
	chain := withdrawals.RPC{Client: c.seq}
	if err := c.until("for the payer to pay it", func() (bool, string, error) {
		rec, err := chain.Withdrawal(ctx, d.Vault, w.ID)
		if err != nil {
			return false, "", err
		}
		var status, lastErr string
		if err := c.auditor.QueryRow(ctx, "SELECT status, coalesce(last_error, '') FROM withdrawals WHERE id = $1", w.ID.Bytes()).Scan(&status, &lastErr); err != nil {
			return false, "", err
		}
		return rec.Status == withdrawals.Paid, fmt.Sprintf("status %s %s", status, lastErr), nil
	}); err != nil {
		return err
	}
	got, err := c.seq.BalanceAt(ctx, to, nil)
	if err != nil {
		return err
	}
	if got.Cmp(amount) != 0 {
		return fmt.Errorf("the recipient holds %s wei, want %s", got, amount)
	}
	var block uint64
	if err := c.until("for the payment to be read", func() (bool, string, error) {
		err := c.auditor.QueryRow(ctx, "SELECT block_number FROM withdrawal_events WHERE withdrawal_id = $1 AND kind = 'paid' AND status <> 'voided'",
			w.ID.Bytes()).Scan(&block)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "not yet read by the indexer", nil
		}
		return err == nil, "", err
	}); err != nil {
		return err
	}
	ok("paid by the payer from the vault", fmt.Sprintf("after %s, block %d; the recipient holds 0.04 ETH", since(requested), block))
	if held, owned, err = c.balances(); err != nil {
		return err
	} else if held.Cmp(amount) != 0 || owned.Cmp(deposit) != 0 {
		return fmt.Errorf("paid, not final: held %s, owned %s wei; want it still held", held, owned)
	}
	ok("still held while the payment is not final", "0.04 ETH held")

	// 4. Settled at finality, not before.
	paid := time.Now()
	if err := c.until("for the payment to be final", func() (bool, string, error) {
		var status string
		if err := c.auditor.QueryRow(ctx, "SELECT status FROM withdrawals WHERE id = $1", w.ID.Bytes()).Scan(&status); err != nil {
			return false, "", err
		}
		fin, err := c.finalized()
		if err != nil {
			return false, "", err
		}
		if status == "settled" && fin < block {
			return false, "", fmt.Errorf("settled while the verifier's finalized block %d is below the payment's block %d", fin, block)
		}
		return status == "settled", fmt.Sprintf("status %s, finalized %d, %d blocks to go", status, fin, int64(block)-int64(fin)), nil
	}); err != nil {
		return err
	}
	held, owned, err = c.balances()
	if err != nil {
		return err
	}
	if held.Sign() != 0 || owned.Cmp(left) != 0 {
		return fmt.Errorf("settled: held %s, owned %s wei; want nothing held and %s owned", held, owned, left)
	}
	ok("settled once final: the hold posted", fmt.Sprintf("after %s; the account owns 0.06 ETH", since(paid)))

	// 5. The books.
	rows, err := c.auditor.Query(ctx, "SELECT problem FROM ledger_check()")
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
	ok("the books agree with the journal", fmt.Sprintf("ledger_check: none; %s in all", since(c.start)))
	return nil
}
