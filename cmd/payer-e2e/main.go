// Command payer-e2e runs the withdrawal payer against a real node that can be made to
// reorganise and to raise its fees: anvil, which make check-bridge starts with a
// PostgreSQL database of its own. It deploys the vault as on the devnet, credits a
// customer through the deposit indexer, and then, each through the vault on chain:
//
//  1. a withdrawal is paid once, and the payer then only watches;
//  2. a send that fails after the transaction was recorded is sent again later, the
//     same bytes, and paid once;
//  3. a payment a reorganisation removed is paid again, once, by the same transaction;
//  4. two payers with one key pay six withdrawals at once, each once, with no nonce
//     used twice or skipped;
//  5. a transaction the base fee left behind is replaced under the same nonce, and only
//     the replacement is mined;
//  6. a large withdrawal is queued by the vault and paid with executeQueued after its
//     delay;
//  7. once final, the indexer settles every withdrawal, the payer closes every
//     transaction, and the ledger equals what the vault on chain accounts for, to the
//     wei.
//
// It connects as three logins, one per role: the requester, the payer and the indexer.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/anvil"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/vaultconf"
	"github.com/WebOleg/settled/internal/withdrawals"
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "anvil's JSON-RPC")
	outDir := flag.String("out", "contracts/out", "forge build output")
	recordRoot := flag.String("record", "", "if set, a folder to write the vault's deployment record under, as vault-deploy does, for bridge-check")
	var dbs [3]struct{ url, password *string }
	for i, role := range []string{"requester", "payer", "indexer"} {
		dbs[i].url = flag.String(role+"-db", "", "the ledger database as the "+role+"'s login, without the password")
		dbs[i].password = flag.String(role+"-db-password-file", "", "file holding the "+role+"'s password")
	}
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var conns [3]*ledger.Ledger
	for i := range dbs {
		dsn, err := ledger.DSN(*dbs[i].url, *dbs[i].password)
		if err != nil {
			fail(err)
		}
		if conns[i], err = ledger.Open(ctx, dsn); err != nil {
			fail(err)
		}
		defer conns[i].Close()
	}
	if err := run(ctx, *rpcURL, *outDir, *recordRoot, conns[0], conns[1], conns[2]); err != nil {
		fail(err)
	}
	fmt.Println("payer-e2e: ok")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "payer-e2e: FAIL: %v\n", err)
	os.Exit(1)
}

func ok(name, detail string) { fmt.Printf("ok    %-50s %s\n", name, detail) }

// milli is n thousandths of an ETH.
func milli(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }

// flaky is the node with its next sends failing, as a connection lost after the
// transaction was recorded and before the node had it.
type flaky struct {
	withdrawals.Chain
	fail int
}

func (f *flaky) Send(ctx context.Context, tx *types.Transaction) error {
	if f.fail > 0 {
		f.fail--
		return errors.New("connection reset by peer")
	}
	return f.Chain.Send(ctx, tx)
}

type e2e struct {
	ctx       context.Context
	client    *ethclient.Client
	vault     anvil.Vault
	requester *ledger.Ledger
	payer     *ledger.Ledger
	indexer   *deposits.Indexer
	operator  *ecdsa.PrivateKey
	account   common.Hash
	paid      *big.Int // what the customer has withdrawn
	ids       []common.Hash
}

func run(ctx context.Context, rpcURL, outDir, recordRoot string, requester, payerDB, indexerDB *ledger.Ledger) error {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	var keys [4]*ecdsa.PrivateKey
	var addrs [4]common.Address
	for i := range keys {
		if keys[i], err = anvil.Key(uint32(i)); err != nil {
			return err
		}
		addrs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}
	vault, err := anvil.DeployVault(ctx, client, outDir, keys[0], anvil.Roles{Admin: addrs[0], Operator: addrs[1], Guardian: addrs[2]}, 30,
		anvil.TokenConfig{Allowed: true, Capacity: anvil.Ether(10), RefillPeriod: 1800, LargeThreshold: anvil.Ether(5)})
	if err != nil {
		return err
	}
	// The rate limit starts empty and refills a capacity every 1,800 blocks.
	if err := anvil.Mine(ctx, client, 1800); err != nil {
		return err
	}
	ok("vault deployed behind its proxy, its limit full", fmt.Sprintf("%s, block %d", vault.At, vault.Block))
	if recordRoot != "" {
		genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
		if err != nil {
			return err
		}
		id, err := client.ChainID(ctx)
		if err != nil {
			return err
		}
		if err := vaultconf.WriteDeployment(recordRoot, &vaultconf.Deployment{ChainID: id.Uint64(), L2Genesis: genesis.Hash(), Vault: vault.At, Block: vault.Block}); err != nil {
			return err
		}
	}

	e := &e2e{ctx: ctx, client: client, vault: vault, requester: requester, payer: payerDB, operator: keys[1],
		account: common.HexToHash("0xbeef"), paid: new(big.Int),
		indexer: &deposits.Indexer{Chain: deposits.RPC{Client: client}, Ledger: indexerDB, Vault: vault.At, Start: vault.Block, MaxRange: 64, MaxBlocks: 2000}}
	if _, err := vault.Deposit(ctx, client, keys[3], e.account, anvil.Ether(8), true); err != nil {
		return err
	}
	if err := anvil.Mine(ctx, client, anvil.Finality+1); err != nil {
		return err
	}
	if err := e.index(); err != nil {
		return err
	}
	if err := e.balance("the customer is credited 8 ETH once final", anvil.Ether(8), new(big.Int)); err != nil {
		return err
	}

	for _, s := range []func() error{e.paidOnce, e.failedSend, e.reorganised, e.twoPayers, e.replaced, e.queued, e.final} {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

func (e *e2e) newPayer(chain withdrawals.Chain, l *ledger.Ledger) *withdrawals.Payer {
	return &withdrawals.Payer{Chain: chain, Ledger: l, Vault: e.vault.At, Key: e.operator, ResendAfter: 0,
		MaxFeeCap: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e9)), Batch: 50}
}

func (e *e2e) rpc() withdrawals.Chain { return withdrawals.RPC{Client: e.client} }

func (e *e2e) step(p *withdrawals.Payer, what string) (withdrawals.Report, error) {
	rep, err := p.Step(e.ctx)
	if err != nil {
		return rep, fmt.Errorf("%s: payer step: %w", what, err)
	}
	return rep, nil
}

// index steps the indexer until it has read up to the node's head.
func (e *e2e) index() error {
	for range 100 {
		rep, err := e.indexer.Step(e.ctx)
		if err != nil {
			return fmt.Errorf("indexer step: %w", err)
		}
		if rep.Tip == rep.Head {
			return nil
		}
	}
	return errors.New("the indexer did not reach the head")
}

func (e *e2e) submit(key string, to common.Address, amount *big.Int) (common.Hash, error) {
	w, err := withdrawals.Submit(e.ctx, e.requester, e.vault.At, withdrawals.Request{Key: key, Account: e.account, Token: deposits.Native, Recipient: to, Amount: amount})
	if err != nil {
		return common.Hash{}, fmt.Errorf("submit %s: %w", key, err)
	}
	e.paid.Add(e.paid, amount)
	e.ids = append(e.ids, w.ID)
	return w.ID, nil
}

// recipient is a fresh address for each withdrawal, so its balance is what was paid.
func recipient(name string) common.Address {
	return common.BytesToAddress(crypto.Keccak256([]byte("payer-e2e recipient " + name))[12:])
}

func (e *e2e) received(to common.Address, want *big.Int) error {
	got, err := e.client.BalanceAt(e.ctx, to, nil)
	if err != nil {
		return err
	}
	if got.Cmp(want) != 0 {
		return fmt.Errorf("%s received %s wei, want %s", to, got, want)
	}
	return nil
}

func (e *e2e) onChain(id common.Hash, want withdrawals.Status) error {
	got, err := e.rpc().Withdrawal(e.ctx, e.vault.At, id)
	if err != nil {
		return err
	}
	if got.Status != want {
		return fmt.Errorf("the vault records %s as %d, want %d", id, got.Status, want)
	}
	return nil
}

// versions counts the transactions recorded for a withdrawal, and its slots (nonces).
func (e *e2e) versions(id common.Hash) (txs, slots int, err error) {
	err = e.payer.Pool().QueryRow(e.ctx, "SELECT count(*), count(DISTINCT nonce) FROM withdrawal_txs WHERE withdrawal_id = $1", id.Bytes()).Scan(&txs, &slots)
	return txs, slots, err
}

func (e *e2e) balance(what string, credited, held *big.Int) error {
	a, err := e.requester.Account(e.ctx, deposits.CustomerAccount(e.account, "ETH"))
	if err != nil {
		return err
	}
	owned := new(big.Int).Sub(a.CreditsPosted, a.DebitsPosted)
	if owned.Cmp(credited) != 0 || a.DebitsPending.Cmp(held) != 0 {
		return fmt.Errorf("%s: owned %s, held %s wei; want %s and %s", what, owned, a.DebitsPending, credited, held)
	}
	ok(what, fmt.Sprintf("owned %s wei, held %s", owned, held))
	return nil
}

// 1. Paid once, then only watched.
func (e *e2e) paidOnce() error {
	to := recipient("once")
	id, err := e.submit("once", to, milli(500))
	if err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if rep, err := e.step(p, "once"); err != nil {
		return err
	} else if rep.Signed != 1 {
		return fmt.Errorf("first step: %+v, want 1 signed", rep)
	}
	for range 3 {
		if rep, err := e.step(p, "once, watching"); err != nil {
			return err
		} else if rep.Signed != 0 || rep.Bumped != 0 {
			return fmt.Errorf("a step after the payment: %+v, want nothing signed", rep)
		}
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(500)); err != nil {
		return err
	}
	ok("a withdrawal is paid once, then only watched", "0.5 ETH, one transaction")
	return nil
}

// 2. The send fails after the transaction was recorded.
func (e *e2e) failedSend() error {
	to := recipient("failed send")
	id, err := e.submit("failed send", to, milli(400))
	if err != nil {
		return err
	}
	if _, err := e.newPayer(&flaky{Chain: e.rpc(), fail: 1}, e.payer).Step(e.ctx); err == nil || !strings.Contains(err.Error(), "connection reset") {
		return fmt.Errorf("the failing send: %v, want the connection error", err)
	}
	if err := e.onChain(id, withdrawals.None); err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if rep, err := e.step(p, "after the failed send"); err != nil {
		return err
	} else if rep.Resent != 1 || rep.Signed != 0 {
		return fmt.Errorf("after the failed send: %+v, want the recorded transaction sent and nothing signed", rep)
	}
	if _, err := e.step(p, "after the resend"); err != nil {
		return err
	}
	txs, _, err := e.versions(id)
	if err != nil {
		return err
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(400)); err != nil {
		return err
	}
	if txs != 1 {
		return fmt.Errorf("%d transactions recorded, want the one", txs)
	}
	ok("a failed send is sent again, the same bytes", "0.4 ETH, one transaction")
	return nil
}

// 3. The block with the payment leaves the chain.
func (e *e2e) reorganised() error {
	to := recipient("reorganised")
	id, err := e.submit("reorganised", to, milli(300))
	if err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if _, err := e.step(p, "reorganised"); err != nil {
		return err
	}
	if rep, err := e.step(p, "reorganised, on chain"); err != nil {
		return err
	} else if rep.OnChain != 1 {
		return fmt.Errorf("before the reorganisation: %+v, want it on chain", rep)
	}
	if err := anvil.Reorg(e.ctx, e.client, 1, nil); err != nil {
		return err
	}
	if err := e.onChain(id, withdrawals.None); err != nil {
		return fmt.Errorf("after anvil_reorg: %w", err)
	}
	var signed, reopened, resent int
	for range 3 {
		rep, err := e.step(p, "after the reorganisation")
		if err != nil {
			return err
		}
		signed, reopened, resent = signed+rep.Signed, reopened+rep.Reopened, resent+rep.Resent
	}
	txs, _, err := e.versions(id)
	if err != nil {
		return err
	}
	if signed != 0 || reopened != 1 || resent != 1 || txs != 1 {
		return fmt.Errorf("after the reorganisation: %d signed, %d reopened, %d resent, %d transactions; want 0, 1, 1, 1", signed, reopened, resent, txs)
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(300)); err != nil {
		return err
	}
	ok("a payment removed by anvil_reorg is paid again, once", "reopened, the same transaction sent")
	return nil
}

// 4. Two payers with one key, at once.
func (e *e2e) twoPayers() error {
	var tos []common.Address
	for i := range 6 {
		to := recipient(fmt.Sprintf("together %d", i))
		if _, err := e.submit(fmt.Sprintf("together %d", i), to, milli(100)); err != nil {
			return err
		}
		tos = append(tos, to)
	}
	before, err := e.client.NonceAt(e.ctx, crypto.PubkeyToAddress(e.operator.PublicKey), nil)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		l, err := ledger.Open(e.ctx, e.payer.Pool().Config().ConnString())
		if err != nil {
			return err
		}
		defer l.Close()
		p := e.newPayer(e.rpc(), l)
		p.Batch = 2 // each takes a few at a time, so both work at once
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := p.Step(e.ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return fmt.Errorf("two payers: %w", err)
	}
	p := e.newPayer(e.rpc(), e.payer)
	for range 2 {
		if _, err := e.step(p, "after two payers"); err != nil {
			return err
		}
	}
	for _, to := range tos {
		if err := e.received(to, milli(100)); err != nil {
			return err
		}
	}
	after, err := e.client.NonceAt(e.ctx, crypto.PubkeyToAddress(e.operator.PublicKey), nil)
	if err != nil {
		return err
	}
	var slots, nonces int
	var low, high uint64
	if err := e.payer.Pool().QueryRow(e.ctx, `SELECT count(*), count(DISTINCT nonce), min(nonce), max(nonce) FROM withdrawal_txs
		WHERE replaces IS NULL AND nonce >= $1`, before).Scan(&slots, &nonces, &low, &high); err != nil {
		return err
	}
	if slots != 6 || nonces != 6 || low != before || high != before+5 || after != before+6 {
		return fmt.Errorf("two payers: %d slots under %d nonces from %d to %d, account nonce %d to %d; want 6 from %d to %d", slots, nonces, low, high, before, after, before, before+5)
	}
	ok("two payers with one key pay six, each once", fmt.Sprintf("nonces %d to %d, none twice or skipped", low, high))
	return nil
}

// 5. The base fee rises past what a sent transaction offers.
func (e *e2e) replaced() error {
	if err := anvil.Automine(e.ctx, e.client, false); err != nil {
		return err
	}
	defer func() { _ = anvil.Automine(context.WithoutCancel(e.ctx), e.client, true) }()
	to := recipient("replaced")
	id, err := e.submit("replaced", to, milli(200))
	if err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if rep, err := e.step(p, "replaced"); err != nil {
		return err
	} else if rep.Signed != 1 {
		return fmt.Errorf("first step: %+v, want 1 signed", rep)
	}
	var first common.Hash
	var firstCap string
	var hash []byte
	if err := e.payer.Pool().QueryRow(e.ctx, "SELECT hash, fee_cap::text FROM withdrawal_txs WHERE withdrawal_id = $1", id.Bytes()).Scan(&hash, &firstCap); err != nil {
		return err
	}
	first = common.BytesToHash(hash)
	// A rush elsewhere: the next block's base fee is far above what it offers.
	high := new(big.Int).Mul(big.NewInt(100), big.NewInt(1e9))
	if err := anvil.NextBaseFee(e.ctx, e.client, high); err != nil {
		return err
	}
	if err := anvil.Mine(e.ctx, e.client, 1); err != nil {
		return err
	}
	if r, err := e.rpc().Receipt(e.ctx, first); err != nil || r != nil {
		return fmt.Errorf("the first version, offering %s wei, was mined under a base fee of %s: %v %v", firstCap, high, r, err)
	}
	p.BumpAfter = time.Second
	time.Sleep(1100 * time.Millisecond)
	if rep, err := e.step(p, "after BumpAfter"); err != nil {
		return err
	} else if rep.Bumped != 1 || rep.Signed != 0 {
		return fmt.Errorf("after BumpAfter: %+v, want it replaced", rep)
	}
	if err := anvil.Mine(e.ctx, e.client, 1); err != nil {
		return err
	}
	p.BumpAfter = 0
	if _, err := e.step(p, "after the replacement was mined"); err != nil {
		return err
	}
	var second []byte
	var secondCap string
	if err := e.payer.Pool().QueryRow(e.ctx, "SELECT hash, fee_cap::text FROM withdrawal_txs WHERE withdrawal_id = $1 AND replaces IS NOT NULL", id.Bytes()).Scan(&second, &secondCap); err != nil {
		return fmt.Errorf("read the replacement: %w", err)
	}
	r, err := e.rpc().Receipt(e.ctx, common.BytesToHash(second))
	if err != nil {
		return err
	}
	if r == nil || !r.OK {
		return fmt.Errorf("the replacement was not mined: %+v", r)
	}
	if r, err := e.rpc().Receipt(e.ctx, first); err != nil || r != nil {
		return fmt.Errorf("the first version was mined too: %v %v", r, err)
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(200)); err != nil {
		return err
	}
	ok("a transaction the base fee left behind is replaced", fmt.Sprintf("fee cap %s wei, then %s; only the second mined", firstCap, secondCap))
	return nil
}

// 6. A large withdrawal waits in the vault's queue.
func (e *e2e) queued() error {
	to := recipient("queued")
	id, err := e.submit("queued", to, anvil.Ether(5))
	if err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if _, err := e.step(p, "queued"); err != nil {
		return err
	}
	if err := e.onChain(id, withdrawals.Queued); err != nil {
		return err
	}
	if rep, err := e.step(p, "queued, before its release"); err != nil {
		return err
	} else if rep.Signed != 0 || rep.Waiting != 1 {
		return fmt.Errorf("before the release block: %+v, want it waiting", rep)
	}
	if err := anvil.Mine(e.ctx, e.client, 30); err != nil {
		return err
	}
	if rep, err := e.step(p, "queued, released"); err != nil {
		return err
	} else if rep.Signed != 1 {
		return fmt.Errorf("at the release block: %+v, want executeQueued signed", rep)
	}
	if _, err := e.step(p, "queued, paid"); err != nil {
		return err
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, anvil.Ether(5)); err != nil {
		return err
	}
	ok("a large withdrawal is paid after the vault's delay", "5 ETH: queued, then executeQueued")
	return nil
}

// 7. Final: settled, closed, and equal to the vault.
func (e *e2e) final() error {
	if err := e.balance("held while not final", anvil.Ether(8), e.paid); err != nil {
		return err
	}
	if err := anvil.Mine(e.ctx, e.client, anvil.Finality+1); err != nil {
		return err
	}
	if err := e.index(); err != nil {
		return err
	}
	p := e.newPayer(e.rpc(), e.payer)
	if _, err := e.step(p, "once final"); err != nil {
		return err
	}
	var settled, openTxs int
	if err := e.payer.Pool().QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE status = 'settled'),
		(SELECT count(*) FROM withdrawal_txs WHERE NOT done) FROM withdrawals WHERE vault = $1`, e.vault.At.Bytes()).Scan(&settled, &openTxs); err != nil {
		return err
	}
	if settled != len(e.ids) || openTxs != 0 {
		return fmt.Errorf("once final: %d of %d settled, %d transactions open; want all settled and none open", settled, len(e.ids), openTxs)
	}
	ok("once final, every withdrawal settled", fmt.Sprintf("%d withdrawals, every transaction closed", settled))
	if err := e.balance("the customer has paid for each, once", new(big.Int).Sub(anvil.Ether(8), e.paid), new(big.Int)); err != nil {
		return err
	}
	problems, err := e.requester.Check(e.ctx)
	if err != nil {
		return err
	}
	if len(problems) != 0 {
		return fmt.Errorf("the books disagree with the journal: %v", problems)
	}
	if _, err := e.requester.Account(e.ctx, deposits.SuspenseAccount("ETH")); !errors.Is(err, ledger.ErrNotFound) {
		return fmt.Errorf("suspense: %v, want no account: every payment was the ledger's", err)
	}
	fin, err := e.client.HeaderByNumber(e.ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return err
	}
	call, err := e.vault.ABI.Call("accounted", deposits.Native)
	if err != nil {
		return err
	}
	out, err := e.client.CallContract(e.ctx, ethereum.CallMsg{To: &e.vault.At, Data: call}, fin.Number)
	if err != nil {
		return fmt.Errorf("accounted(ETH): %w", err)
	}
	v, err := e.requester.Account(e.ctx, "vault:ETH")
	if err != nil {
		return err
	}
	held := new(big.Int).Sub(v.DebitsPosted, v.CreditsPosted)
	if onChain := new(big.Int).SetBytes(out); held.Cmp(onChain) != 0 {
		return fmt.Errorf("the ledger holds %s wei for the vault, the vault accounts for %s at block %s", held, onChain, fin.Number)
	}
	ok("ledger equals the vault on chain", fmt.Sprintf("%s wei at finalized block %s; none in suspense", held, fin.Number))
	return nil
}
