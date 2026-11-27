// Command withdrawal-signer signs for the withdrawal payer (internal/signer), over TLS
// with client certificates, only what its policy and the ledger allow, and records every
// decision in the ledger's signer_log before it answers. The key it signs with stays in
// a KMS that speaks the AWS KMS API, reached with the AWS SDK as a principal that may
// only read its public key and sign with it: AWS KMS in production, kms-dev on the
// devnet. A key in a file (-key-file) is for runs without a KMS. It reads the vault and
// its chain from the deployment record and the ledger as a login that holds only
// withdrawal_signer; it reaches no node. It serves Prometheus metrics on a port of its
// own. Credentials and the password come from files and are never printed.
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/WebOleg/settled/internal/awscreds"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/signer"
	"github.com/WebOleg/settled/internal/vaultwatch"
)

type config struct {
	record, db, passwordFile, keyFile, tlsDir, listen, metrics string
	kmsEndpoint, kmsRegion, kmsKey, kmsCredentials             string
	maxGas                                                     uint64
	maxFeeCap, limit                                           *big.Int
	window                                                     time.Duration
}

func main() {
	var c config
	flag.StringVar(&c.record, "record", "/vault/deployment.json", "the vault's deployment record")
	flag.StringVar(&c.db, "db", "postgres://signer@postgres:5432/ledger?sslmode=disable", "the ledger database, without the password")
	flag.StringVar(&c.passwordFile, "db-password-file", "/run/secrets/signer_db", "file holding the database password")
	flag.StringVar(&c.keyFile, "key-file", "", "file holding the payer's private key, hex, when there is no KMS")
	flag.StringVar(&c.kmsEndpoint, "kms-endpoint", "", "the KMS's endpoint; empty for AWS KMS itself in -kms-region")
	flag.StringVar(&c.kmsRegion, "kms-region", "eu-central-1", "the KMS's region")
	flag.StringVar(&c.kmsKey, "kms-key", "alias/payer", "the key in the KMS, by id, ARN or alias")
	flag.StringVar(&c.kmsCredentials, "kms-credentials", "/run/secrets/kms_signer", "file holding the signer's access key id and secret for the KMS")
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

// key is the signer's key: in the KMS, or with -key-file, in a file.
func key(ctx context.Context, c config) (signer.Key, error) {
	if c.keyFile != "" {
		return signer.ReadFileKey(c.keyFile)
	}
	id, secret, err := awscreds.Read(c.kmsCredentials)
	if err != nil {
		return nil, err
	}
	opts := kms.Options{Region: c.kmsRegion, Credentials: credentials.NewStaticCredentialsProvider(id, secret, "")}
	if c.kmsEndpoint != "" {
		opts.BaseEndpoint = aws.String(c.kmsEndpoint)
	}
	// The KMS may start after the signer: it is asked again until it answers.
	for i := 0; ; i++ {
		k, err := signer.NewKMSKey(ctx, kms.New(opts), c.kmsKey)
		if err == nil || i == 30 {
			return k, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func run(ctx context.Context, c config) error {
	key, err := key(ctx, c)
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
	// The vault and its chain, from the deployment record: until it names this key as the
	// payer's, nothing can be signed and the signer waits.
	rec, err := waitRecord(ctx, c.record, key.Address())
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

// waitRecord waits for a deployment record that names as the payer's the account the
// signer's key controls: the signer signs for no other.
func waitRecord(ctx context.Context, path string, as common.Address) (vaultwatch.Record, error) {
	said := ""
	for {
		rec, ok, err := vaultwatch.ReadRecord(path)
		if err != nil {
			return rec, err
		}
		why := "no vault is deployed yet"
		switch {
		case ok && rec.Payer == nil:
			why = "the vault has given no payer the operator role yet (make vault-payer)"
		case ok && !common.IsHexAddress(rec.Payer.Address):
			return rec, fmt.Errorf("the deployment record names the payer %q, not an address", rec.Payer.Address)
		case ok && common.HexToAddress(rec.Payer.Address) != as:
			why = fmt.Sprintf("the vault's payer is %s, not this key's account %s (make vault-payer)", rec.Payer.Address, as)
		case ok:
			return rec, nil
		}
		if why != said {
			log.Printf("withdrawal-signer: waiting: %s", why)
			said = why
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
