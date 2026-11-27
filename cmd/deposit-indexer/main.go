// Command deposit-indexer keeps the ledger in step with the vault's deposits
// (internal/deposits): held when seen on the L2, credited once final, voided if a
// reorganisation removes them first. It reads the chain from the verifier node, finds
// the vault in the deployment record, read again at every step, and serves what it does
// as Prometheus metrics. The database password comes from a file and is never printed.
//
// When the node's chain no longer has a block the indexer held final, it stops
// writing, reports it in deposit_indexer_finality_violation and waits for a person.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/deposits"
	"github.com/WebOleg/settled/internal/ledger"
	"github.com/WebOleg/settled/internal/vaultwatch"
)

func main() {
	rpcURL := flag.String("rpc", "http://verifier-op-reth:8545", "execution JSON-RPC of the L2 verifier")
	record := flag.String("record", "/vault/deployment.json", "the vault's deployment record")
	db := flag.String("db", "postgres://indexer@postgres:5432/ledger?sslmode=disable", "the ledger database, without the password")
	passwordFile := flag.String("db-password-file", "/run/secrets/indexer_db", "file holding the database password")
	listen := flag.String("listen", ":9110", "address to serve /metrics on")
	every := flag.Duration("poll", 2*time.Second, "time between steps; the L2 makes a block every 2 s")
	checkEvery := flag.Duration("check", 30*time.Second, "time between checks of the books against the journal")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *rpcURL, *record, *db, *passwordFile, *listen, *every, *checkEvery); err != nil {
		log.Fatalf("deposit-indexer: %v", err)
	}
}

func run(ctx context.Context, rpcURL, record, db, passwordFile, listen string, every, checkEvery time.Duration) error {
	dsn, err := ledger.DSN(db, passwordFile)
	if err != nil {
		return err
	}
	l, err := openLedger(ctx, dsn)
	if err != nil {
		return err
	}
	defer l.Close()
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()

	m := &metrics{}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := m.write(rw); err != nil {
			log.Printf("deposit-indexer: write metrics: %v", err)
		}
	})
	mux.HandleFunc("/-/healthy", func(rw http.ResponseWriter, _ *http.Request) { fmt.Fprintln(rw, "ok") })
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()

	chain := deposits.RPC{Client: client}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var lastErr string
	var lastCheck time.Time
	for {
		if err := step(ctx, m, chain, l, record); err != nil {
			if err.Error() != lastErr {
				log.Printf("deposit-indexer: step: %v", err)
				lastErr = err.Error()
			}
		} else if lastErr != "" {
			log.Printf("deposit-indexer: steps succeed again")
			lastErr = ""
		}
		if time.Since(lastCheck) >= checkEvery {
			lastCheck = time.Now()
			if err := check(ctx, m, l); err != nil {
				log.Printf("deposit-indexer: check the books: %v", err)
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

func step(ctx context.Context, m *metrics, chain deposits.Chain, l *ledger.Ledger, record string) error {
	if m.violation() {
		return deposits.ErrFinalityViolation
	}
	rec, ok, err := vaultwatch.ReadRecord(record)
	if err != nil {
		m.failed()
		return err
	}
	if !ok {
		m.noVault()
		return nil
	}
	x := &deposits.Indexer{Chain: chain, Ledger: l, Vault: common.HexToAddress(rec.Vault), Start: rec.Block, MaxRange: 2000, MaxBlocks: 300}
	rep, err := x.Step(ctx)
	if errors.Is(err, deposits.ErrFinalityViolation) {
		m.violate()
		log.Printf("deposit-indexer: STOPPED: %v; nothing more is written until a person has looked", err)
		return err
	}
	if err != nil {
		m.failed()
		return err
	}
	m.stepped(rep)
	if rep.Seen > 0 || rep.Posted > 0 || rep.Voided > 0 || rep.Fork != nil {
		fork := "-"
		if rep.Fork != nil {
			fork = fmt.Sprint(*rep.Fork)
		}
		log.Printf("deposit-indexer: tip %d head %d finalized %d: %d seen, %d posted, %d voided, fork %s",
			rep.Tip, rep.Head, rep.Finalized, rep.Seen, rep.Posted, rep.Voided, fork)
	}
	return nil
}

// check runs the ledger's own check and counts what is still held.
func check(ctx context.Context, m *metrics, l *ledger.Ledger) error {
	problems, err := l.Check(ctx)
	if err != nil {
		return err
	}
	for _, p := range problems {
		log.Printf("deposit-indexer: the books disagree with the journal: %s", p)
	}
	var pending int64
	if err := l.Pool().QueryRow(ctx, "SELECT count(*) FROM deposits WHERE status = 'pending'").Scan(&pending); err != nil {
		return fmt.Errorf("count pending deposits: %w", err)
	}
	m.checked(len(problems), pending)
	return nil
}
