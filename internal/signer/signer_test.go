package signer

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/ledger/ledgertest"
	"github.com/WebOleg/settled/internal/withdrawals"
)

var (
	vault   = common.HexToAddress("0x00000000000000000000000000000000000000aa")
	chainID = big.NewInt(901)
	alice   = common.HexToHash("0xa11ce")
	bob     = common.HexToAddress("0xb0b")
)

func eth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func milli(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e15)) }

type fixture struct {
	requester *ledger.Ledger
	owner     *pgxpool.Pool
	service   *Service
	key       *ecdsa.PrivateKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db := ledgertest.New(t, ledger.Migrate, "withdrawal_requester")
	signerURL := db.Login(t, "withdrawal_signer")
	requester, err := ledger.Open(ctx, db.AppURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(requester.Close)
	pool, err := pgxpool.New(ctx, signerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	owner, err := pgxpool.New(ctx, db.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{requester: requester, owner: owner, key: key, service: &Service{Key: NewFileKey(key), DB: pool, Policy: Policy{
		ChainID: chainID, Vault: vault, MaxGas: 300_000, MaxFeeCap: big.NewInt(100e9), Window: time.Hour, Limit: eth(2),
	}}}
	v, err := requester.CreateAccount(ctx, "vault:ETH", "ETH", ledger.CreditsWithinDebits)
	if err != nil {
		t.Fatal(err)
	}
	c, err := requester.CreateAccount(ctx, deposits.CustomerAccount(alice, "ETH"), "ETH", ledger.DebitsWithinCredits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requester.Transfer(ctx, ledger.Transfer{Key: "deposit", Kind: ledger.Single, Debit: v.ID, Credit: c.ID, Amount: eth(100), Code: "deposit"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) submit(t *testing.T, key string, amount *big.Int) withdrawals.Withdrawal {
	t.Helper()
	w, err := withdrawals.Submit(context.Background(), f.requester, vault, withdrawals.Request{Key: key, Account: alice, Token: deposits.Native, Recipient: bob, Amount: amount})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// call is the unsigned transaction the payer would ask for.
func call(data []byte, nonce uint64) *types.DynamicFeeTx {
	return &types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(2e9), Gas: 100_000, To: &vault, Data: data}
}

func withdraw(t *testing.T, id common.Hash, to common.Address, amount *big.Int) []byte {
	t.Helper()
	data, err := withdrawals.VaultABI.Pack("withdraw", id, deposits.Native, to, amount)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *fixture) decisions(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), "SELECT decision || ':' || rule FROM signer_log")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out[s]++
	}
	return out
}

func TestSign_whatTheLedgerHoldsIsSignedAndRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.submit(t, "w-1", eth(1))
	signed, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, w.ID, bob, eth(1)), 7)))
	if err != nil {
		t.Fatal(err)
	}
	from, err := types.Sender(types.LatestSignerForChainID(chainID), signed)
	if err != nil || from != crypto.PubkeyToAddress(f.key.PublicKey) || signed.Nonce() != 7 {
		t.Fatalf("signed by %s, nonce %d, %v; want the key's account and nonce 7", from, signed.Nonce(), err)
	}
	var hash []byte
	var client, method string
	if err := f.owner.QueryRow(ctx, "SELECT tx_hash, client, method FROM signer_log WHERE decision = 'signed'").Scan(&hash, &client, &method); err != nil {
		t.Fatal(err)
	}
	if common.BytesToHash(hash) != signed.Hash() || client != ClientName || method != "withdraw" {
		t.Fatalf("recorded %x by %s for %s; want the signed hash by %s for withdraw", hash, client, method, ClientName)
	}
	queue, err := withdrawals.VaultABI.Pack("executeQueued", w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Sign(ctx, ClientName, types.NewTx(call(queue, 8))); err != nil {
		t.Fatalf("executeQueued of an open withdrawal: %v", err)
	}
}

func TestSign_whatThePolicyForbidsIsRefusedAndRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.submit(t, "w-1", eth(1))
	good := withdraw(t, w.ID, bob, eth(1))
	other := common.HexToAddress("0xbad")
	pause, _ := withdrawals.VaultABI.Pack("executeQueued", w.ID)
	pause = append([]byte{0x84, 0x56, 0xcb, 0x59}, pause[4:]...) // pause()'s selector
	settled := f.submit(t, "w-settled", milli(100))
	if _, err := f.owner.Exec(ctx, "UPDATE withdrawals SET status = 'onchain' WHERE id = $1", settled.ID.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, "UPDATE withdrawals SET status = 'settled' WHERE id = $1", settled.ID.Bytes()); err != nil {
		t.Fatal(err)
	}
	unknown := crypto.Keccak256Hash([]byte("nobody asked"))
	for _, c := range []struct {
		rule string
		tx   *types.Transaction
	}{
		{"type", types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1e9), Gas: 100_000, To: &vault, Data: good})},
		{"chain", func() *types.Transaction { c := call(good, 1); c.ChainID = big.NewInt(1); return types.NewTx(c) }()},
		{"destination", func() *types.Transaction { c := call(good, 1); c.To = &other; return types.NewTx(c) }()},
		{"value", func() *types.Transaction { c := call(good, 1); c.Value = big.NewInt(1); return types.NewTx(c) }()},
		{"access list", func() *types.Transaction {
			c := call(good, 1)
			c.AccessList = types.AccessList{{Address: vault}}
			return types.NewTx(c)
		}()},
		{"gas", func() *types.Transaction { c := call(good, 1); c.Gas = 10_000_000; return types.NewTx(c) }()},
		{"fee cap", func() *types.Transaction { c := call(good, 1); c.GasFeeCap = big.NewInt(1000e9); return types.NewTx(c) }()},
		{"method", types.NewTx(call(pause, 1))},
		{"method", types.NewTx(call(append(append([]byte{}, good...), 0), 1))},
		{"ledger", types.NewTx(call(withdraw(t, unknown, bob, eth(1)), 1))},
		{"ledger", types.NewTx(call(withdraw(t, w.ID, other, eth(1)), 1))},
		{"ledger", types.NewTx(call(withdraw(t, w.ID, bob, eth(2)), 1))},
		{"ledger", types.NewTx(call(withdraw(t, settled.ID, bob, milli(100)), 1))},
	} {
		_, err := f.service.Sign(ctx, ClientName, c.tx)
		var refused *withdrawals.Refused
		if !errors.As(err, &refused) || refused.Rule != c.rule {
			t.Errorf("%s: %v, want it refused under %s", c.rule, err, c.rule)
		}
	}
	got := f.decisions(t)
	if got["signed:allowed"] != 0 || got["refused:ledger"] != 4 || got["refused:method"] != 2 || got["refused:type"] != 1 {
		t.Fatalf("decisions recorded: %v; want each refusal, nothing signed", got)
	}
}

// The velocity limit counts each withdrawal once, however often it is signed again.
func TestSign_theVelocityLimitCountsEachWithdrawalOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w1 := f.submit(t, "w-1", milli(1500))
	w2 := f.submit(t, "w-2", milli(600))
	if _, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, w1.ID, bob, milli(1500)), 1))); err != nil {
		t.Fatal(err)
	}
	// A replacement of w1, a tenth more in fees: no new volume.
	r := call(withdraw(t, w1.ID, bob, milli(1500)), 1)
	r.GasTipCap, r.GasFeeCap = big.NewInt(2e9), big.NewInt(3e9)
	if _, err := f.service.Sign(ctx, ClientName, types.NewTx(r)); err != nil {
		t.Fatalf("a replacement of a withdrawal signed in the window: %v", err)
	}
	_, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, w2.ID, bob, milli(600)), 2)))
	var refused *withdrawals.Refused
	if !errors.As(err, &refused) || refused.Rule != "velocity" {
		t.Fatalf("2.1 ETH in the window, at most 2: %v, want it refused under velocity", err)
	}
	// The window moves on: w1 signed more than an hour ago no longer counts.
	if _, err := f.owner.Exec(ctx, "ALTER TABLE signer_log DISABLE TRIGGER signer_log_append_only"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, "UPDATE signer_log SET at = at - interval '2 hours'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Exec(ctx, "ALTER TABLE signer_log ENABLE TRIGGER signer_log_append_only"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, w2.ID, bob, milli(600)), 2))); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// Signers at once spend the limit once: of ten withdrawals of 0.3 ETH under a 2 ETH
// limit, six are signed.
func TestSign_signersAtOnceSpendTheLimitOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var ids []common.Hash
	for i := range 10 {
		ids = append(ids, f.submit(t, fmt.Sprintf("w-%d", i), milli(300)).ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	signed, refused := 0, 0
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, id, bob, milli(300)), uint64(i))))
			mu.Lock()
			defer mu.Unlock()
			var r *withdrawals.Refused
			switch {
			case err == nil:
				signed++
			case errors.As(err, &r) && r.Rule == "velocity":
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if signed != 6 || refused != 4 {
		t.Fatalf("%d signed, %d refused; want 6 and 4", signed, refused)
	}
}

func TestSignerLog_isAppendOnlyAndTheSignerChangesNothingElse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.submit(t, "w-1", eth(1))
	if _, err := f.service.Sign(ctx, ClientName, types.NewTx(call(withdraw(t, w.ID, bob, eth(1)), 1))); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"UPDATE signer_log SET rule = 'x'", "DELETE FROM signer_log", "TRUNCATE signer_log"} {
		if _, err := f.owner.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s as the owner: %v, want it refused", q, err)
		}
	}
	for _, q := range []string{"UPDATE withdrawals SET status = 'settled'", "UPDATE signer_log SET rule = 'x'", "SELECT * FROM accounts"} {
		if _, err := f.service.DB.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s as the signer: %v, want permission denied", q, err)
		}
	}
}

// Over HTTPS with client certificates: the payer's certificate signs; another issued by
// the same CA under another name is refused and recorded; a client without one does
// not get through TLS.
func TestHandler_onlyTheAllowedClientSigns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTLS(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	serverTLS, err := ServerTLS(filepath.Join(dir, CAFile), filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(Handler(f.service, map[string]bool{ClientName: true}))
	srv.TLS = serverTLS
	srv.StartTLS()
	defer srv.Close()
	url := "https://" + srv.Listener.Addr().String()

	clientTLS, err := ClientTLS(filepath.Join(dir, CAFile), filepath.Join(dir, ClientCertFile), filepath.Join(dir, ClientKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	clientTLS.ServerName = "localhost"
	remote, err := NewRemote(ctx, url, clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	if remote.Address() != crypto.PubkeyToAddress(f.key.PublicKey) {
		t.Fatalf("the signer signs for %s, want the key's account", remote.Address())
	}
	w := f.submit(t, "w-1", eth(1))
	if _, err := remote.Sign(ctx, types.NewTx(call(withdraw(t, w.ID, bob, eth(1)), 1))); err != nil {
		t.Fatalf("the payer's certificate: %v", err)
	}
	_, err = remote.Sign(ctx, types.NewTx(call(withdraw(t, w.ID, bob, eth(9)), 1)))
	var refused *withdrawals.Refused
	if !errors.As(err, &refused) || refused.Rule != "ledger" {
		t.Fatalf("an amount the ledger does not hold, over HTTPS: %v, want it refused under ledger", err)
	}

	intruder := intruderTLS(t, dir)
	intruder.ServerName = "localhost"
	other, err := NewRemote(ctx, url, intruder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Sign(ctx, types.NewTx(call(withdraw(t, w.ID, bob, eth(1)), 1))); !errors.As(err, &refused) || refused.Rule != "client" {
		t.Fatalf("a certificate of another name: %v, want it refused under client", err)
	}
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: clientTLS.RootCAs, ServerName: "localhost", MinVersion: tls.VersionTLS13}}}
	if resp, err := bare.Get(url + "/v1/address"); err == nil {
		resp.Body.Close()
		t.Fatal("a client without a certificate got through TLS")
	}
	if got := f.decisions(t); got["refused:client"] != 1 || got["signed:allowed"] != 1 || got["refused:ledger"] != 1 {
		t.Fatalf("decisions recorded: %v", got)
	}
}

// intruderTLS is a client certificate the signer's own CA issued, under another name.
func intruderTLS(t *testing.T, dir string) *tls.Config {
	t.Helper()
	readPEM := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(raw)
		return block.Bytes
	}
	ca, err := x509.ParseCertificate(readPEM(CAFile))
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := x509.ParseECPrivateKey(readPEM(caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(caKey.Curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "intruder"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func TestEnsureTLS_madeOnceAndAgainBeforeItEnds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureTLS(dir, time.Now()); err == nil {
		t.Fatal("a folder others can enter: accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if wrote, err := EnsureTLS(dir, now); err != nil || !wrote {
		t.Fatalf("first: %t, %v", wrote, err)
	}
	if info, err := os.Stat(filepath.Join(dir, caKeyFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the CA's key: %v, %v; want 600", info.Mode().Perm(), err)
	}
	if wrote, err := EnsureTLS(dir, now.Add(300*24*time.Hour)); err != nil || wrote {
		t.Fatalf("300 days later: %t, %v; want nothing written", wrote, err)
	}
	if wrote, err := EnsureTLS(dir, now.Add(340*24*time.Hour)); err != nil || !wrote {
		t.Fatalf("340 days later, within 30 days of the end: %t, %v; want it made again", wrote, err)
	}
}
