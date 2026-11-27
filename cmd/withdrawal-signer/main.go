// Command withdrawal-signer is the only holder of the payer's key (internal/signer). It
// signs for the withdrawal payer, over TLS with client certificates, only what its
// policy and the ledger allow, and records every decision in the ledger's signer_log
// before it answers. It reads the vault and its chain from the deployment record and
// the ledger as a login that holds only withdrawal_signer; it reaches no node. It
// serves Prometheus metrics on a port of its own. The key and the password come from
// files and are never printed.
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/signer"
	"github.com/WebOleg/settled/internal/vaultwatch"
)

type config struct {
	record, db, passwordFile, keyFile, tlsDir, listen, metrics string
	maxGas                                                     uint64
	maxFeeCap, limit                                           *big.Int
	window                                                     time.Duration
}

func main() {
	var c config
	flag.StringVar(&c.record, "record", "/vault/deployment.json", "the vault's deployment record")
	flag.StringVar(&c.db, "db", "postgres://signer@postgres:5432/ledger?sslmode=disable", "the ledger database, without the password")
	flag.StringVar(&c.passwordFile, "db-password-file", "/run/secrets/signer_db", "file holding the database password")
	flag.StringVar(&c.keyFile, "key-file", "/run/secrets/signing_key", "file holding the payer's private key, hex")
	flag.StringVar(&c.tlsDir, "tls", "/run/secrets", "folder holding ca.pem, signer.pem and signer-key.pem")
	flag.StringVar(&c.listen, "listen", ":9443", "address to serve the signing API on, over TLS")
	flag.StringVar(&c.metrics, "metrics", ":9112", "address to serve /metrics on")
	flag.Uint64Var(&c.maxGas, "max-gas", 300_000, "the most gas a transaction may carry")
	maxFeeCap := flag.Uint64("max-fee-cap-gwei", 100, "the most a transaction may offer per gas, in gwei")
	limit := flag.Uint64("limit-milli-eth", 5000, "what withdraw may pay per token over a window, in thousandths of an ETH")
	flag.DurationVar(&c.window, "window", time.Hour, "the window of the velocity limit")
	flag.Parse()
	c.maxFeeCap = new(big.Int).Mul(new(big.Int).SetUint64(*maxFeeCap), big.NewInt(1e9))
	c.limit = new(big.Int).Mul(new(big.Int).SetUint64(*limit), big.NewInt(1e15))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, c); err != nil {
		log.Fatalf("withdrawal-signer: %v", err)
	}
}

func run(ctx context.Context, c config) error {
	key, err := signer.ReadFileKey(c.keyFile)
	if err != nil {
		return err
	}
	dsn, err := ledger.DSN(c.db, c.passwordFile)
	if err != nil {
		return err
	}
	pool, err := openPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	tlsConfig, err := signer.ServerTLS(filepath.Join(c.tlsDir, signer.CAFile), filepath.Join(c.tlsDir, signer.ServerCertFile), filepath.Join(c.tlsDir, signer.ServerKeyFile))
	if err != nil {
		return err
	}
	// The vault and its chain, from the deployment record: until there is one, nothing
	// can be signed and the signer waits.
	rec, err := waitRecord(ctx, c.record)
	if err != nil {
		return err
	}
	s := &signer.Service{Key: key, DB: pool, Policy: signer.Policy{
		ChainID: new(big.Int).SetUint64(rec.ChainID), Vault: common.HexToAddress(rec.Vault),
		MaxGas: c.maxGas, MaxFeeCap: c.maxFeeCap, Window: c.window, Limit: c.limit,
	}}
	log.Printf("withdrawal-signer: signing as %s for the vault %s on chain %d; velocity limit %s wei per %s",
		key.Address(), s.Policy.Vault, rec.ChainID, c.limit, c.window)

	m := newMetrics()
	s.Observe = m.decided
	api := &http.Server{Addr: c.listen, Handler: signer.Handler(s, map[string]bool{signer.ClientName: true}), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := m.write(rw); err != nil {
			log.Printf("withdrawal-signer: write metrics: %v", err)
		}
	})
	mux.HandleFunc("/-/healthy", func(rw http.ResponseWriter, _ *http.Request) { fmt.Fprintln(rw, "ok") })
	metrics := &http.Server{Addr: c.metrics, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 2)
	go func() { served <- api.ListenAndServeTLS("", "") }()
	go func() { served <- metrics.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(api.Shutdown(shutdown), metrics.Shutdown(shutdown))
	case err := <-served:
		return err
	}
}

func waitRecord(ctx context.Context, path string) (vaultwatch.Record, error) {
	for {
		rec, ok, err := vaultwatch.ReadRecord(path)
		if err != nil {
			return rec, err
		}
		if ok {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// openPool waits for the database, which may still be starting.
func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	for i := 0; ; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
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
