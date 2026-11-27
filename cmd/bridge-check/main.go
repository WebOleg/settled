// Command bridge-check compares the ledger with the chain from outside the indexer that
// keeps it, as a reconciliation does: it reads the vault's events itself from the
// verifier node and the books as an auditor that can only read (ledger_reader). Up to
// the last block the indexer holds final:
//
//   - every Deposited event of the vault is a posted deposit in the ledger, with its
//     amount and account, and the ledger has no posted deposit the chain does not;
//   - no deposit at or below that block is still held;
//   - each customer's credited balance is the sum of the chain's deposits for it;
//   - the ledger's books agree with its journal (ledger_check);
//   - what the ledger holds for the vault is the sum of the deposits, and the vault's
//     own accounted(ETH) is that sum less the withdrawals it paid, to the wei.
//
// It changes nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/vaultconf"
)

// topicPaid is keccak256 of the vault's WithdrawalPaid event (IVault).
var topicPaid = crypto.Keccak256Hash([]byte("WithdrawalPaid(bytes32,address,address,uint256)"))

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	db := flag.String("db", "postgres://auditor@127.0.0.1:15432/ledger?sslmode=disable", "the ledger database, without the password")
	passwordFile := flag.String("db-password-file", ".devnet/bridge/auditor.txt", "file holding the database password")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir, *db, *passwordFile); err != nil {
		fmt.Fprintf(os.Stderr, "bridge-check: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("bridge-check: ok")
}

func ok(name, detail string) { fmt.Printf("ok    %-50s %s\n", name, detail) }

type key struct {
	block common.Hash
	index uint
}

type entry struct {
	account common.Hash
	amount  *big.Int
	number  uint64
}

func run(ctx context.Context, rpcURL, outDir, db, passwordFile string) error {
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return fmt.Errorf("%w (run make vault-deploy)", err)
	}
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return fmt.Errorf("the deployment record belongs to another L2")
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

	// The indexer keeps up: its tip within a minute of blocks of the node's head.
	var tip, final uint64
	for i := 0; ; i++ {
		head, err := client.BlockNumber(ctx)
		if err != nil {
			return err
		}
		if err := conn.QueryRow(ctx, `SELECT coalesce(max(number), 0), coalesce(max(number) FILTER (WHERE final), 0)
			FROM indexed_blocks WHERE vault = $1`, d.Vault.Bytes()).Scan(&tip, &final); err != nil {
			return fmt.Errorf("read the indexer's blocks: %w", err)
		}
		if tip+30 >= head {
			ok("the indexer keeps up with the node", fmt.Sprintf("tip %d, head %d, final up to %d", tip, head, final))
			break
		}
		if i == 30 {
			return fmt.Errorf("the indexer's tip %d is %d blocks behind the head %d after a minute", tip, head-tip, head)
		}
		time.Sleep(2 * time.Second)
	}
	if final < d.Block {
		return fmt.Errorf("the indexer holds nothing final since the vault's deployment block %d yet", d.Block)
	}

	// The books, in one snapshot that the rest of the check reads from.
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := tx.QueryRow(ctx, "SELECT coalesce(max(number) FILTER (WHERE final), 0) FROM indexed_blocks WHERE vault = $1",
		d.Vault.Bytes()).Scan(&final); err != nil {
		return err
	}
	var stillHeld int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM deposits WHERE vault = $1 AND status = 'pending' AND block_number <= $2",
		d.Vault.Bytes(), final).Scan(&stillHeld); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT block_hash, log_index, block_number, account, amount::text FROM deposits WHERE vault = $1 AND status = 'posted'", d.Vault.Bytes())
	if err != nil {
		return err
	}
	posted := map[key]entry{}
	for rows.Next() {
		var bh, account []byte
		var idx int
		var e entry
		var amount string
		if err := rows.Scan(&bh, &idx, &e.number, &account, &amount); err != nil {
			return err
		}
		e.account = common.BytesToHash(account)
		e.amount, _ = new(big.Int).SetString(amount, 10)
		posted[key{common.BytesToHash(bh), uint(idx)}] = e
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, "SELECT problem FROM ledger_check()")
	if err != nil {
		return fmt.Errorf("ledger_check: %w", err)
	}
	problems, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("ledger_check: %w", err)
	}
	// balance reads what the ledger holds for an account: posted credits less posted
	// debits for a customer, the other way round for the vault.
	balance := func(name string, owed bool) (*big.Int, error) {
		q := "SELECT (debits_posted - credits_posted)::text FROM accounts WHERE name = $1"
		if owed {
			q = "SELECT (credits_posted - debits_posted)::text FROM accounts WHERE name = $1"
		}
		var v string
		err := tx.QueryRow(ctx, q, name).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return new(big.Int), nil
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		b, _ := new(big.Int).SetString(v, 10)
		return b, nil
	}

	// The chain, up to the same block.
	chain := map[key]entry{}
	perAccount := map[common.Hash]*big.Int{}
	total, paid := new(big.Int), new(big.Int)
	payments := 0
	for from := d.Block; from <= final; from += 2000 {
		to := min(final, from+1999)
		events, err := deposits.RPC{Client: client}.EventsInRange(ctx, d.Vault, from, to)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Token != deposits.Native {
				return fmt.Errorf("a deposit of token %s at block %d; the devnet vault takes ETH only", e.Token, e.Block)
			}
			chain[key{e.BlockHash, e.LogIndex}] = entry{e.Account, e.Amount, e.Block}
			total.Add(total, e.Amount)
			if perAccount[e.Account] == nil {
				perAccount[e.Account] = new(big.Int)
			}
			perAccount[e.Account].Add(perAccount[e.Account], e.Amount)
		}
		logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to),
			Addresses: []common.Address{d.Vault}, Topics: [][]common.Hash{{topicPaid}, nil, {common.BytesToHash(deposits.Native.Bytes())}}})
		if err != nil {
			return fmt.Errorf("read WithdrawalPaid: %w", err)
		}
		for _, l := range logs {
			if len(l.Data) != 32 {
				return fmt.Errorf("WithdrawalPaid at block %d has %d bytes of data", l.BlockNumber, len(l.Data))
			}
			paid.Add(paid, new(big.Int).SetBytes(l.Data))
			payments++
		}
	}

	// Event by event.
	for k, c := range chain {
		p, found := posted[k]
		if !found {
			return fmt.Errorf("the deposit at block %d (%s, log %d) is not posted in the ledger", c.number, k.block, k.index)
		}
		if p.amount.Cmp(c.amount) != 0 || p.account != c.account {
			return fmt.Errorf("the deposit at block %d: ledger %s wei for %s, chain %s wei for %s", c.number, p.amount, p.account, c.amount, c.account)
		}
	}
	for k, p := range posted {
		if _, found := chain[k]; !found {
			return fmt.Errorf("the ledger posted a deposit at block %d (%s, log %d) the chain does not have", p.number, k.block, k.index)
		}
	}
	ok("every deposit on chain is posted, and only those", fmt.Sprintf("%d deposits, %s wei, up to block %d", len(chain), total, final))
	if stillHeld != 0 {
		return fmt.Errorf("%d deposits at or below the final block %d are still held", stillHeld, final)
	}
	ok("none final is still held", "0")

	accounts := make([]common.Hash, 0, len(perAccount))
	for a := range perAccount {
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Hex() < accounts[j].Hex() })
	for _, a := range accounts {
		got, err := balance(deposits.CustomerAccount(a, "ETH"), true)
		if err != nil {
			return err
		}
		if got.Cmp(perAccount[a]) != 0 {
			return fmt.Errorf("customer %s is credited %v wei, the chain deposited %s", a, got, perAccount[a])
		}
	}
	ok("each customer is credited what the chain deposited", fmt.Sprintf("%d accounts", len(accounts)))

	if len(problems) != 0 {
		return fmt.Errorf("the books disagree with the journal: %v", problems)
	}
	ok("the books agree with the journal", "ledger_check: none")

	vaultHeld, err := balance("vault:ETH", false)
	if err != nil {
		return err
	}
	if vaultHeld.Cmp(total) != 0 {
		return fmt.Errorf("the ledger holds %s wei for the vault, the chain deposited %s", vaultHeld, total)
	}
	call, err := vault.Call("accounted", deposits.Native)
	if err != nil {
		return err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &d.Vault, Data: call}, new(big.Int).SetUint64(final))
	if err != nil {
		return fmt.Errorf("accounted(ETH) at block %d: %w", final, err)
	}
	accounted := new(big.Int).SetBytes(out)
	if want := new(big.Int).Sub(total, paid); accounted.Cmp(want) != 0 {
		return fmt.Errorf("the vault accounts for %s wei at block %d; deposits less payments are %s", accounted, final, want)
	}
	ok("ledger = deposits; vault = deposits - payments", fmt.Sprintf("%s wei held, %d payments of %s wei, accounted %s", vaultHeld, payments, paid, accounted))
	return nil
}
