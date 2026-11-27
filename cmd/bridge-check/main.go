// Command bridge-check compares the ledger with the chain from outside the indexer that
// keeps it, as a reconciliation does: it reads the vault's events itself from the
// verifier node and the books as an auditor that can only read (ledger_reader). Up to
// the last block the indexer holds final:
//
//   - every Deposited event of the vault is a posted deposit in the ledger, with its
//     amount and account, and the ledger has no posted deposit the chain does not;
//   - no deposit at or below that block is still held;
//   - each customer's credited balance is the sum of the chain's deposits for it;
//   - every WithdrawalPaid event is applied: it settled a withdrawal of the ledger or,
//     paid without the ledger, went to suspense, and nothing else is applied; no
//     payment or cancellation at or below that block is still pending;
//   - suspense holds those payments less the ones explained: each approved resolution
//     moved its whole payment out to the account it names, and nothing else left;
//   - the ledger's books agree with its journal (ledger_check);
//   - what the ledger holds for the vault equals the vault's own accounted(ETH), to the
//     wei, and is the deposits less the payments.
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
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/vaultconf"
)

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
	// Payments and cancellations are compared up to the same block, unless the indexer
	// is still backfilling them (a stream added after blocks were read): then up to
	// the last block backfilled, which is below every block they are read live in.
	horizon := final
	var liveFrom, backfilledTo uint64
	if err := tx.QueryRow(ctx, "SELECT live_from, backfilled_to FROM indexer_streams WHERE vault = $1 AND stream = 'withdrawals'",
		d.Vault.Bytes()).Scan(&liveFrom, &backfilledTo); err != nil {
		return fmt.Errorf("read the indexer's withdrawals stream: %w", err)
	}
	backfilling := backfilledTo+1 < liveFrom
	if backfilling {
		horizon = min(final, backfilledTo)
	}
	// Payments and cancellations as the indexer applied them.
	var eventsPending int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM withdrawal_events WHERE vault = $1 AND status = 'pending' AND block_number <= $2",
		d.Vault.Bytes(), horizon).Scan(&eventsPending); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT block_hash, log_index, block_number, amount::text, outcome FROM withdrawal_events
		WHERE vault = $1 AND status = 'final' AND kind = 'paid'`, d.Vault.Bytes())
	if err != nil {
		return err
	}
	type applied struct {
		amount  *big.Int
		number  uint64
		outcome string
	}
	appliedPaid := map[key]applied{}
	for rows.Next() {
		var bh []byte
		var idx int
		var a applied
		var amount string
		if err := rows.Scan(&bh, &idx, &a.number, &amount, &a.outcome); err != nil {
			return err
		}
		a.amount, _ = new(big.Int).SetString(amount, 10)
		appliedPaid[key{common.BytesToHash(bh), uint(idx)}] = a
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
	chainPaid := map[key]entry{}
	for from := d.Block; from <= final; from += 2000 {
		to := min(final, from+1999)
		events, err := deposits.RPC{Client: client}.EventsInRange(ctx, d.Vault, from, to)
		if err != nil {
			return err
		}
		for _, e := range events {
			if e.Kind == deposits.KindCancelled {
				continue // moves no money
			}
			if e.Token != deposits.Native {
				return fmt.Errorf("a %s of token %s at block %d; the devnet vault takes ETH only", e.Kind, e.Token, e.Block)
			}
			if e.Kind == deposits.KindPaid {
				if e.Block <= horizon {
					chainPaid[key{e.BlockHash, e.LogIndex}] = entry{amount: e.Amount, number: e.Block}
					paid.Add(paid, e.Amount)
				}
				continue
			}
			chain[key{e.BlockHash, e.LogIndex}] = entry{e.Account, e.Amount, e.Block}
			total.Add(total, e.Amount)
			if perAccount[e.Account] == nil {
				perAccount[e.Account] = new(big.Int)
			}
			perAccount[e.Account].Add(perAccount[e.Account], e.Amount)
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

	// Payment by payment.
	settled, toSuspense := 0, new(big.Int)
	for k, c := range chainPaid {
		a, found := appliedPaid[k]
		if !found {
			return fmt.Errorf("the payment at block %d (%s, log %d) is not applied in the ledger", c.number, k.block, k.index)
		}
		if a.amount.Cmp(c.amount) != 0 {
			return fmt.Errorf("the payment at block %d: ledger %s wei, chain %s wei", c.number, a.amount, c.amount)
		}
		switch a.outcome {
		case "settled":
			settled++
		case "suspense":
			toSuspense.Add(toSuspense, a.amount)
		default:
			return fmt.Errorf("the payment at block %d was applied as %q", c.number, a.outcome)
		}
	}
	for k, a := range appliedPaid {
		if _, found := chainPaid[k]; !found {
			return fmt.Errorf("the ledger applied a payment at block %d (%s, log %d) the chain does not have", a.number, k.block, k.index)
		}
	}
	if eventsPending != 0 {
		return fmt.Errorf("%d payments or cancellations at or below block %d are still pending", eventsPending, horizon)
	}
	if backfilling {
		ok("withdrawal events backfilled so far", fmt.Sprintf("up to block %d of %d; read live from %d", backfilledTo, liveFrom-1, liveFrom))
	}
	ok("every payment on chain is applied, and only those", fmt.Sprintf("%d payments, %s wei: %d settled withdrawals, %d to suspense", len(chainPaid), paid, settled, len(chainPaid)-settled))
	// Each approved resolution moved its whole payment out of suspense, once, to the
	// account it names, and nothing else left suspense.
	var resolutions, badResolutions int
	resolved := "0"
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(r.amount), 0)::text,
		count(*) FILTER (WHERE t.id IS NULL OR t.kind <> 'single' OR t.amount <> r.amount OR t.debit_account <> r.account
			OR t.code <> 'suspense-' || r.kind OR s.name IS DISTINCT FROM $1 OR e.outcome IS DISTINCT FROM 'suspense' OR e.vault <> $2)
		FROM suspense_resolutions r
		LEFT JOIN transfers t ON t.id = r.transfer_id
		LEFT JOIN accounts s ON s.id = t.credit_account
		LEFT JOIN withdrawal_events e ON e.id = r.event_id
		WHERE r.status = 'approved'`, deposits.SuspenseAccount("ETH"), d.Vault.Bytes()).Scan(&resolutions, &resolved, &badResolutions); err != nil {
		return fmt.Errorf("read the resolutions of suspense: %w", err)
	}
	if badResolutions != 0 {
		return fmt.Errorf("%d approved resolutions of suspense do not match the transfer they made", badResolutions)
	}
	var outOfSuspense string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(t.amount), 0)::text FROM transfers t JOIN accounts s ON s.id = t.credit_account
		WHERE s.name = $1`, deposits.SuspenseAccount("ETH")).Scan(&outOfSuspense); err != nil {
		return fmt.Errorf("read what left suspense: %w", err)
	}
	if outOfSuspense != resolved {
		return fmt.Errorf("%s wei left suspense, the approved resolutions moved %s", outOfSuspense, resolved)
	}
	resolvedWei, _ := new(big.Int).SetString(resolved, 10)
	suspense, err := balance(deposits.SuspenseAccount("ETH"), false)
	if err != nil {
		return err
	}
	if want := new(big.Int).Sub(toSuspense, resolvedWei); suspense.Cmp(want) != 0 {
		return fmt.Errorf("the suspense account holds %s wei; the payments posted to it less those resolved are %s", suspense, want)
	}
	ok("suspense holds the payments not yet explained", fmt.Sprintf("%s wei; %d explained under four eyes, %s wei", suspense, resolutions, resolvedWei))

	vaultHeld, err := balance("vault:ETH", false)
	if err != nil {
		return err
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
	if want := new(big.Int).Sub(total, paid); vaultHeld.Cmp(want) != 0 {
		return fmt.Errorf("the ledger holds %s wei for the vault; deposits up to block %d less payments up to block %d are %s", vaultHeld, final, horizon, want)
	}
	if backfilling {
		ok("ledger = deposits less the payments backfilled", fmt.Sprintf("%s wei; equal to the vault once the backfill reaches block %d", vaultHeld, liveFrom-1))
		return nil
	}
	if vaultHeld.Cmp(accounted) != 0 {
		return fmt.Errorf("the ledger holds %s wei for the vault, the vault accounts for %s at block %d", vaultHeld, accounted, final)
	}
	ok("ledger holds exactly what the vault accounts for", fmt.Sprintf("%s wei at block %d", vaultHeld, final))
	return nil
}
