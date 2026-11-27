// Command withdrawal-payer pays the ledger's withdrawals from the vault
// (internal/withdrawals): it brings the vault to each open withdrawal, signing a
// transaction only where the vault has nothing for its id, records every transaction
// before sending it, sends it again until it is final, and replaces one the network's
// fees left behind. It finds the vault in the deployment record, read again at every
// step, and serves what it does as Prometheus metrics. It holds no key: the withdrawal
// signer (cmd/withdrawal-signer) signs for it over TLS with its client certificate, and
// only what the signer's policy and the ledger allow. The database password comes from
// a file and is never printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/signer"
	"github.com/WebOleg/settled/internal/vaultwatch"
	"github.com/WebOleg/settled/internal/withdrawals"
)

type config struct {
	rpc, record, db, passwordFile, signerURL, tlsDir, listen string
	every, resendAfter, bumpAfter, checkEvery                time.Duration
	maxFeeCap                                                *big.Int
	batch                                                    int
}

func main() {
	var c config
	flag.StringVar(&c.rpc, "rpc", "http://op-reth:8545", "execution JSON-RPC of the L2 node the payer sends to")
	flag.StringVar(&c.record, "record", "/vault/deployment.json", "the vault's deployment record")
	flag.StringVar(&c.db, "db", "postgres://payer@postgres:5432/ledger?sslmode=disable", "the ledger database, without the password")
	flag.StringVar(&c.passwordFile, "db-password-file", "/run/secrets/payer_db", "file holding the database password")
	flag.StringVar(&c.signerURL, "signer", "https://withdrawal-signer:9443", "the withdrawal signer's API")
	flag.StringVar(&c.tlsDir, "tls", "/run/secrets", "folder holding ca.pem, payer.pem and payer-key.pem")
	flag.StringVar(&c.listen, "listen", ":9111", "address to serve /metrics on")
	flag.DurationVar(&c.every, "poll", 2*time.Second, "time between steps; the L2 makes a block every 2 s")
	flag.DurationVar(&c.resendAfter, "resend-after", 30*time.Second, "how long a sent transaction may go unmined before it is sent again")
	flag.DurationVar(&c.bumpAfter, "bump-after", 2*time.Minute, "how long a transaction may go unmined before it is replaced with higher fees")
	flag.DurationVar(&c.checkEvery, "check", 15*time.Second, "time between readings of what waits")
	maxFeeCap := flag.Uint64("max-fee-cap-gwei", 100, "the most a transaction may offer per gas, in gwei")
	flag.IntVar(&c.batch, "batch", 50, "the most withdrawals one step works on")
	flag.Parse()
	c.maxFeeCap = new(big.Int).Mul(new(big.Int).SetUint64(*maxFeeCap), big.NewInt(1e9))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c); err != nil {
		log.Fatalf("withdrawal-payer: %v", err)
	}
}

func run(ctx context.Context, c config) error {
	if c.maxFeeCap.Sign() <= 0 || c.bumpAfter <= c.resendAfter {
		return errors.New("-max-fee-cap-gwei must be positive and -bump-after longer than -resend-after")
	}
	tlsConfig, err := signer.ClientTLS(filepath.Join(c.tlsDir, signer.CAFile), filepath.Join(c.tlsDir, signer.ClientCertFile), filepath.Join(c.tlsDir, signer.ClientKeyFile))
	if err != nil {
		return err
	}
	dsn, err := ledger.DSN(c.db, c.passwordFile)
	if err != nil {
		return err
	}
	l, err := openLedger(ctx, dsn)
	if err != nil {
		return err
	}
	defer l.Close()
	client, err := ethclient.DialContext(ctx, c.rpc)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.rpc, err)
	}
	defer client.Close()

	m := &metrics{}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := m.write(rw); err != nil {
			log.Printf("withdrawal-payer: write metrics: %v", err)
		}
	})
	mux.HandleFunc("/-/healthy", func(rw http.ResponseWriter, _ *http.Request) { fmt.Fprintln(rw, "ok") })
	srv := &http.Server{Addr: c.listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()

	chain := withdrawals.RPC{Client: client}
	tick := time.NewTicker(c.every)
	defer tick.Stop()
	var lastErr string
	var lastCheck time.Time
	var remote *signer.Remote
	for {
		// The signer may start after the payer: it is reached again until it answers. Its
		// account is read again at each step, so a rotated key is followed: the nonces
		// are the new account's from then on.
		if remote == nil {
			if remote, err = signer.NewRemote(ctx, c.signerURL, tlsConfig); err == nil {
				log.Printf("withdrawal-payer: the signer at %s signs for %s", c.signerURL, remote.Address())
			}
		} else {
			was := remote.Address()
			var changed bool
			if changed, err = remote.Refresh(ctx); changed {
				log.Printf("withdrawal-payer: the signer now signs for %s, no longer for %s", remote.Address(), was)
			}
		}
		var vault common.Address
		if remote == nil || err != nil {
			m.failed()
		} else {
			vault, err = step(ctx, m, chain, l, remote, c)
		}
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("withdrawal-payer: step: %v", err)
				lastErr = err.Error()
			}
		} else if lastErr != "" {
			log.Printf("withdrawal-payer: steps succeed again")
			lastErr = ""
		}
		if vault != (common.Address{}) && time.Since(lastCheck) >= c.checkEvery {
			lastCheck = time.Now()
			if err := check(ctx, m, l, client, vault, remote.Address()); err != nil {
				log.Printf("withdrawal-payer: read what waits: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdown)
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-tick.C:
		}
	}
}

// openLedger waits for the database, which may still be starting.
func openLedger(ctx context.Context, dsn string) (*ledger.Ledger, error) {
	for i := 0; ; i++ {
		l, err := ledger.Open(ctx, dsn)
		if err == nil {
			return l, nil
		}
		if i == 30 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func step(ctx context.Context, m *metrics, chain withdrawals.Chain, l *ledger.Ledger, s withdrawals.Signer, c config) (common.Address, error) {
	rec, ok, err := vaultwatch.ReadRecord(c.record)
	if err != nil {
		m.failed()
		return common.Address{}, err
	}
	if !ok {
		m.noVault()
		return common.Address{}, nil
	}
	vault := common.HexToAddress(rec.Vault)
	p := &withdrawals.Payer{Chain: chain, Ledger: l, Vault: vault, Signer: s, ResendAfter: c.resendAfter, BumpAfter: c.bumpAfter,
		MaxFeeCap: c.maxFeeCap, Batch: c.batch}
	rep, err := p.Step(ctx)
	m.stepped(rep, err == nil)
	if err != nil {
		return vault, err
	}
	if rep.Signed > 0 || rep.Resent > 0 || rep.Bumped > 0 || rep.Capped > 0 || rep.Refused > 0 || rep.Rejected > 0 || rep.Reopened > 0 || rep.OnChain > 0 {
		log.Printf("withdrawal-payer: %d signed, %d sent again, %d replaced, %d at the fee cap, %d refused by the signer, %d rejected, %d reopened, %d on chain, %d closed, %d waiting",
			rep.Signed, rep.Resent, rep.Bumped, rep.Capped, rep.Refused, rep.Rejected, rep.Reopened, rep.OnChain, rep.Closed, rep.Waiting)
	}
	return vault, nil
}

// check reads what waits: the open withdrawals and how long the oldest not yet on chain
// has, the transactions open and how long the oldest unmined has, and what the payer's
// key holds for gas.
func check(ctx context.Context, m *metrics, l *ledger.Ledger, client *ethclient.Client, vault, from common.Address) error {
	var w waiting
	err := l.Pool().QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE status = 'requested')), 0)::float8
		FROM withdrawals WHERE vault = $1 AND status IN ('requested', 'onchain')`, vault.Bytes()).Scan(&w.open, &w.oldestRequested)
	if err != nil {
		return fmt.Errorf("read open withdrawals: %w", err)
	}
	if err := l.Pool().QueryRow(ctx, `SELECT count(*) FROM withdrawal_txs t JOIN withdrawals w ON w.id = t.withdrawal_id
		WHERE w.vault = $1 AND NOT t.done`, vault.Bytes()).Scan(&w.openTxs); err != nil {
		return fmt.Errorf("read open transactions: %w", err)
	}
	balance, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		return fmt.Errorf("read the payer's balance: %w", err)
	}
	w.balance = balance
	m.check(w)
	return nil
}
