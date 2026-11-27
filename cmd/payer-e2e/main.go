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
//  7. the signer refuses what the ledger does not hold (another recipient, another
//     amount, another contract, value) and what passes its velocity limit; a payer
//     refused waits, records why, and takes no nonce;
//  8. the payer's key is rotated in the KMS: a signer started before signs nothing, the
//     new key pays, and the old one holds no role and is emptied into the new;
//  9. once final, the indexer settles every withdrawal, the payer closes every
//     transaction, and the ledger equals what the vault on chain accounts for, to the
//     wei.
//
// The payer holds no key: every transaction is signed by the withdrawal signer
// (internal/signer), served here over TLS with client certificates as on the devnet,
// under its policy and against the ledger, with the operator's key in kms-dev
// (internal/kmsdev), reached through the AWS SDK. It connects as four logins, one per
// role: the requester, the payer, the signer and the indexer.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/anvil"
	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/kmsdev"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/signer"
	"github.com/WebOleg/settled/internal/sweep"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/vaultconf"
	"github.com/WebOleg/settled/internal/withdrawals"
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "anvil's JSON-RPC")
	outDir := flag.String("out", "contracts/out", "forge build output")
	recordRoot := flag.String("record", "", "if set, a folder to write the vault's deployment record under, as vault-deploy does, for bridge-check")
	var dbs [4]struct{ url, password *string }
	for i, role := range []string{"requester", "payer", "indexer", "signer"} {
		dbs[i].url = flag.String(role+"-db", "", "the ledger database as the "+role+"'s login, without the password")
		dbs[i].password = flag.String(role+"-db-password-file", "", "file holding the "+role+"'s password")
	}
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var conns [4]*ledger.Ledger
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
	if err := run(ctx, *rpcURL, *outDir, *recordRoot, conns[0], conns[1], conns[2], conns[3]); err != nil {
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
	// service is the signer, remote the payer's way to it over TLS, kms the KMS its key
	// is in, and admin the vault's admin on anvil.
	service *signer.Service
	remote  *signer.Remote
	kms     *kmsRig
	admin   *ecdsa.PrivateKey
	account common.Hash
	paid    *big.Int // what the customer has withdrawn
	ids     []common.Hash
}

func run(ctx context.Context, rpcURL, outDir, recordRoot string, requester, payerDB, indexerDB, signerDB *ledger.Ledger) error {
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
	// The operator's key is made in the KMS and never leaves it; the vault is deployed
	// with its account as the operator, which anvil then funds for gas.
	rig, err := startKMS(ctx)
	if err != nil {
		return err
	}
	defer rig.stop()
	kmsKey := rig.key
	if _, err := txsend.Send(ctx, client, keys[0], kmsKey.Address(), anvil.Ether(10), nil); err != nil {
		return fmt.Errorf("fund the KMS key's account: %w", err)
	}
	ok("the operator's key is made in the KMS", fmt.Sprintf("%s, through the AWS SDK; it never leaves the KMS", kmsKey.Address()))
	vault, err := anvil.DeployVault(ctx, client, outDir, keys[0], anvil.Roles{Admin: addrs[0], Operator: kmsKey.Address(), Guardian: addrs[2]}, 30,
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

	chainID, err := client.ChainID(ctx)
	if err != nil {
		return err
	}
	e := &e2e{ctx: ctx, client: client, vault: vault, requester: requester, payer: payerDB,
		account: common.HexToHash("0xbeef"), paid: new(big.Int), kms: rig, admin: keys[0],
		indexer: &deposits.Indexer{Chain: deposits.RPC{Client: client}, Ledger: indexerDB, Vault: vault.At, Start: vault.Block, MaxRange: 64, MaxBlocks: 2000},
		service: &signer.Service{Key: kmsKey, DB: signerDB.Pool(), Policy: signer.Policy{
			ChainID: chainID, Vault: vault.At, MaxGas: 500_000, MaxFeeCap: new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e9)),
			Window: time.Hour, Limit: anvil.Ether(20),
		}}}
	stop, err := e.serveSigner()
	if err != nil {
		return err
	}
	defer stop()
	ok("the signer serves over TLS, client certificates required", fmt.Sprintf("signs for %s; the payer holds no key", e.remote.Address()))
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

	for _, s := range []func() error{e.paidOnce, e.failedSend, e.reorganised, e.twoPayers, e.replaced, e.queued, e.refused, e.rotated, e.final} {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

// kmsRig is kms-dev on a local port: the operator's key in it, and the admin's and the
// signer's clients, through the AWS SDK, as on the devnet.
type kmsRig struct {
	key                     *signer.KMSKey
	admin, signer, rotation *kms.Client
	stop                    func()
}

// startKMS serves kms-dev on a local port, makes the operator's key in it as the admin,
// and reaches it as the signer.
func startKMS(ctx context.Context) (*kmsRig, error) {
	dir, err := os.MkdirTemp("", "payer-e2e-kms-")
	if err != nil {
		return nil, err
	}
	// Each run makes its own access keys, as bridge-init.sh does for the devnet.
	admin, signerID := "AKIA"+strings.Repeat("A", 16), "AKIA"+strings.Repeat("S", 16)
	adminSecret, signerSecret := rand.Text(), rand.Text()
	rotationID, rotationSecret := "AKIA"+strings.Repeat("R", 16), rand.Text()
	srv := &kmsdev.Server{Region: "eu-central-1", Account: "000000000000", Dir: dir, Principals: []kmsdev.Principal{
		{Name: "admin", AccessKeyID: admin, SecretAccessKey: adminSecret, Actions: []string{"kms:CreateKey", "kms:CreateAlias", "kms:UpdateAlias", "kms:DescribeKey", "kms:GetPublicKey"}},
		{Name: "withdrawal-signer", AccessKeyID: signerID, SecretAccessKey: signerSecret, Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer"}},
		{Name: "key-rotation", AccessKeyID: rotationID, SecretAccessKey: rotationSecret, Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer-retiring"}},
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	stop := func() { _ = hs.Close(); _ = os.RemoveAll(dir) }
	client := func(id, secret string) *kms.Client {
		return kms.New(kms.Options{Region: "eu-central-1", BaseEndpoint: aws.String("http://" + ln.Addr().String()),
			Credentials: credentials.NewStaticCredentialsProvider(id, secret, "")})
	}
	r := &kmsRig{admin: client(admin, adminSecret), signer: client(signerID, signerSecret), rotation: client(rotationID, rotationSecret), stop: stop}
	made, err := r.admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify})
	if err != nil {
		stop()
		return nil, err
	}
	if _, err := r.admin.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String("alias/payer"), TargetKeyId: made.KeyMetadata.KeyId}); err != nil {
		stop()
		return nil, err
	}
	if r.key, err = signer.NewKMSKey(ctx, r.signer, "alias/payer"); err != nil {
		stop()
		return nil, err
	}
	return r, nil
}

// serveSigner serves the signer on a local port over TLS, with the devnet's kind of
// certificates made in a folder of their own, and connects the payer's side to it.
func (e *e2e) serveSigner() (func(), error) {
	dir, err := os.MkdirTemp("", "payer-e2e-tls-")
	if err != nil {
		return nil, err
	}
	if _, err := signer.EnsureTLS(dir, time.Now()); err != nil {
		return nil, err
	}
	serverTLS, err := signer.ServerTLS(filepath.Join(dir, signer.CAFile), filepath.Join(dir, signer.ServerCertFile), filepath.Join(dir, signer.ServerKeyFile))
	if err != nil {
		return nil, err
	}
	clientTLS, err := signer.ClientTLS(filepath.Join(dir, signer.CAFile), filepath.Join(dir, signer.ClientCertFile), filepath.Join(dir, signer.ClientKeyFile))
	if err != nil {
		return nil, err
	}
	clientTLS.ServerName = "localhost"
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: signer.Handler(e.service, map[string]bool{signer.ClientName: true}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop := func() { _ = srv.Close(); _ = os.RemoveAll(dir) }
	if e.remote, err = signer.NewRemote(e.ctx, "https://"+ln.Addr().String(), clientTLS); err != nil {
		stop()
		return nil, err
	}
	return stop, nil
}

func (e *e2e) newPayer(chain withdrawals.Chain, l *ledger.Ledger) *withdrawals.Payer {
	return &withdrawals.Payer{Chain: chain, Ledger: l, Vault: e.vault.At, Signer: e.remote, ResendAfter: 0,
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
	before, err := e.client.NonceAt(e.ctx, e.remote.Address(), nil)
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
	after, err := e.client.NonceAt(e.ctx, e.remote.Address(), nil)
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

// 7. The signer stands between the payer and the key.
func (e *e2e) refused() error {
	to := recipient("refused")
	id, err := e.submit("refused", to, milli(300))
	if err != nil {
		return err
	}
	chainID, err := e.client.ChainID(e.ctx)
	if err != nil {
		return err
	}
	nonce, err := e.client.NonceAt(e.ctx, e.remote.Address(), nil)
	if err != nil {
		return err
	}
	ask := func(to common.Address, data []byte, value *big.Int) error {
		_, err := e.remote.Sign(e.ctx, types.NewTx(&types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1e9),
			GasFeeCap: big.NewInt(10e9), Gas: 200_000, To: &to, Value: value, Data: data}))
		return err
	}
	pack := func(args ...any) []byte {
		data, err := withdrawals.VaultABI.Pack("withdraw", args...)
		if err != nil {
			panic(err)
		}
		return data
	}
	thief := recipient("thief")
	for _, c := range []struct {
		name, rule string
		to         common.Address
		data       []byte
		value      *big.Int
	}{
		{"another recipient", "ledger", e.vault.At, pack(id, deposits.Native, thief, milli(300)), nil},
		{"another amount", "ledger", e.vault.At, pack(id, deposits.Native, to, anvil.Ether(3)), nil},
		{"an id the ledger never requested", "ledger", e.vault.At, pack(crypto.Keccak256Hash([]byte("forged")), deposits.Native, thief, milli(300)), nil},
		{"another contract", "destination", thief, pack(id, deposits.Native, to, milli(300)), nil},
		{"a plain transfer of the key's ETH", "value", e.vault.At, nil, anvil.Ether(1)},
	} {
		err := ask(c.to, c.data, c.value)
		var refused *withdrawals.Refused
		if !errors.As(err, &refused) || refused.Rule != c.rule {
			return fmt.Errorf("the signer asked for %s: %v, want it refused under %s", c.name, err, c.rule)
		}
	}
	ok("the signer refuses what the ledger does not hold", "another recipient, amount, id, contract; a plain transfer")

	// A signer whose velocity limit this withdrawal passes: the payer is refused, waits,
	// and takes no nonce; the signer of the service then pays it.
	strict := *e.service
	strict.Policy.Limit = milli(100)
	p := e.newPayer(e.rpc(), e.payer)
	p.Signer = signer.Local{Service: &strict, Client: signer.ClientName}
	if rep, err := e.step(p, "under a velocity limit of 0.1 ETH"); err != nil {
		return err
	} else if rep.Refused != 1 || rep.Signed != 0 {
		return fmt.Errorf("under a velocity limit of 0.1 ETH: %+v, want it refused", rep)
	}
	if txs, _, err := e.versions(id); err != nil || txs != 0 {
		return fmt.Errorf("after the refusal: %d transactions recorded, %v; want none", txs, err)
	}
	if n, err := e.client.NonceAt(e.ctx, e.remote.Address(), nil); err != nil || n != nonce {
		return fmt.Errorf("after the refusal the key's nonce is %d, %v; want %d", n, err, nonce)
	}
	if _, err := e.step(e.newPayer(e.rpc(), e.payer), "with the service's signer"); err != nil {
		return err
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(300)); err != nil {
		return err
	}
	ok("a payer the velocity limit refuses waits", "nothing recorded, no nonce taken; paid within the limit after")
	return nil
}

// 8. The payer's key is rotated, as make vault-payer-rotate does: a new key in the KMS,
// the old one named alias/payer-retiring and alias/payer moved to the new one, the
// operator role given to its account and taken from the old one, and the old one
// emptied into the new by key rotation. A signer started before the move signs
// nothing; started again, it signs as the new account, and the payer follows it at its
// next step.
func (e *e2e) rotated() error {
	old := e.remote.Address()
	current, err := e.kms.admin.DescribeKey(e.ctx, &kms.DescribeKeyInput{KeyId: aws.String("alias/payer")})
	if err != nil {
		return err
	}
	if _, err := e.kms.admin.CreateAlias(e.ctx, &kms.CreateAliasInput{AliasName: aws.String("alias/payer-retiring"), TargetKeyId: current.KeyMetadata.KeyId}); err != nil {
		return err
	}
	made, err := e.kms.admin.CreateKey(e.ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify})
	if err != nil {
		return err
	}
	if _, err := e.kms.admin.UpdateAlias(e.ctx, &kms.UpdateAliasInput{AliasName: aws.String("alias/payer"), TargetKeyId: made.KeyMetadata.KeyId}); err != nil {
		return err
	}
	if _, err := e.service.Key.SignHash(e.ctx, crypto.Keccak256Hash([]byte("after the move"))); err == nil {
		return errors.New("a signer started before the alias moved still signs, with a key it did not start with")
	}
	next, err := signer.NewKMSKey(e.ctx, e.kms.signer, "alias/payer")
	if err != nil {
		return err
	}
	if next.Address() == old {
		return errors.New("the new key has the old key's account")
	}
	for _, c := range []struct {
		method string
		who    common.Address
	}{{"grantRole", next.Address()}, {"revokeRole", old}} {
		data, err := e.vault.ABI.Call(c.method, vaultconf.OperatorRole, c.who)
		if err != nil {
			return err
		}
		if _, err := txsend.Send(e.ctx, e.client, e.admin, e.vault.At, nil, data); err != nil {
			return fmt.Errorf("%s(OPERATOR_ROLE, %s): %w", c.method, c.who, err)
		}
	}
	if _, err := txsend.Send(e.ctx, e.client, e.admin, next.Address(), anvil.Ether(1), nil); err != nil {
		return err
	}
	// The old key, with no role now, is emptied into the new account by key rotation.
	retiring, err := signer.NewKMSKey(e.ctx, e.kms.rotation, "alias/payer-retiring")
	if err != nil {
		return err
	}
	before, err := e.client.BalanceAt(e.ctx, next.Address(), nil)
	if err != nil {
		return err
	}
	swept, err := sweep.Sweep(e.ctx, e.client, retiring, next.Address())
	if err != nil {
		return err
	}
	after, err := e.client.BalanceAt(e.ctx, next.Address(), nil)
	if err != nil {
		return err
	}
	// What is left is the fee cap above the fee paid: under 21,000 gas at a gwei.
	if swept.Moved.Sign() <= 0 || new(big.Int).Sub(after, before).Cmp(swept.Moved) != 0 || swept.Left.Cmp(big.NewInt(21_000*1e9)) >= 0 {
		return fmt.Errorf("the old key emptied: moved %s, the new account got %s, %s wei left", swept.Moved, new(big.Int).Sub(after, before), swept.Left)
	}
	e.service.Key = next
	if changed, err := e.remote.Refresh(e.ctx); err != nil || !changed || e.remote.Address() != next.Address() {
		return fmt.Errorf("the payer reads the signer's account: %s, changed %t, %v; want %s", e.remote.Address(), changed, err, next.Address())
	}

	to := recipient("rotated")
	id, err := e.submit("rotated", to, milli(200))
	if err != nil {
		return err
	}
	if rep, err := e.step(e.newPayer(e.rpc(), e.payer), "after the rotation"); err != nil {
		return err
	} else if rep.Signed != 1 {
		return fmt.Errorf("after the rotation: %+v, want 1 signed", rep)
	}
	if err := e.onChain(id, withdrawals.Paid); err != nil {
		return err
	}
	if err := e.received(to, milli(200)); err != nil {
		return err
	}
	if n, err := e.client.NonceAt(e.ctx, next.Address(), nil); err != nil || n != 1 {
		return fmt.Errorf("the new account's nonce is %d, %v; want 1, the payment", n, err)
	}
	call, err := e.vault.ABI.Call("hasRole", vaultconf.OperatorRole, old)
	if err != nil {
		return err
	}
	if out, err := e.client.CallContract(e.ctx, ethereum.CallMsg{To: &e.vault.At, Data: call}, nil); err != nil || new(big.Int).SetBytes(out).Sign() != 0 {
		return fmt.Errorf("the old account still holds OPERATOR_ROLE (%x, %v)", out, err)
	}
	ok("the payer's key is rotated in the KMS", fmt.Sprintf("%s to %s; paid by the new key, the old one holds no role, emptied to %s wei", short(old), short(next.Address()), swept.Left))
	return nil
}

func short(a common.Address) string { return a.Hex()[:10] }

// 9. Final: settled, closed, and equal to the vault.
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

	// Every transaction recorded was signed by the signer, and recorded there first.
	rows, err := e.payer.Pool().Query(e.ctx, "SELECT hash FROM withdrawal_txs")
	if err != nil {
		return err
	}
	var hashes [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	var unsigned, signed, refused int
	if err := e.service.DB.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE NOT EXISTS (SELECT FROM signer_log l WHERE l.decision = 'signed' AND l.tx_hash = h)),
		(SELECT count(*) FROM signer_log WHERE decision = 'signed'), (SELECT count(*) FROM signer_log WHERE decision = 'refused')
		FROM unnest($1::bytea[]) h`, hashes).Scan(&unsigned, &signed, &refused); err != nil {
		return err
	}
	if unsigned != 0 || signed < len(hashes) || refused != 6 {
		return fmt.Errorf("signer_log: %d of %d transactions without a signature recorded, %d signed, %d refused; want none, at least %d, 6",
			unsigned, len(hashes), signed, refused, len(hashes))
	}
	ok("every transaction was signed by the signer, recorded", fmt.Sprintf("%d transactions, %d signatures, %d refusals in signer_log", len(hashes), signed, refused))
	return nil
}
