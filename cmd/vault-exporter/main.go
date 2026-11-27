// Command vault-exporter watches the vault on the L2 and serves what it sees as
// Prometheus metrics (internal/vaultwatch): withdrawals waiting in the queue, the vault
// paused, the vault holding less ETH than it owes. It runs next to Prometheus, reads the
// chain from the verifier node, which derives it from the L1, and finds the vault in the
// deployment record, read again at every poll, so a new deployment is picked up without
// a restart.
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

	"github.com/WebOleg/settled/internal/vaultwatch"
)

func main() {
	rpcURL := flag.String("rpc", "http://verifier-op-reth:8545", "execution JSON-RPC of the L2 verifier")
	record := flag.String("record", "/vault/deployment.json", "the vault's deployment record")
	listen := flag.String("listen", ":9100", "address to serve /metrics on")
	every := flag.Duration("poll", 2*time.Second, "time between polls; the L2 makes a block every 2 s")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w := &vaultwatch.Watcher{Client: &vaultwatch.Client{URL: *rpcURL, HTTP: &http.Client{Timeout: 10 * time.Second}}, RecordPath: *record}
	if err := run(ctx, w, *listen, *every); err != nil {
		log.Fatalf("vault-exporter: %v", err)
	}
}

func run(ctx context.Context, w *vaultwatch.Watcher, listen string, every time.Duration) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := w.WriteMetrics(rw); err != nil {
			log.Printf("vault-exporter: write metrics: %v", err)
		}
	})
	mux.HandleFunc("/-/healthy", func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(rw, "ok")
	})
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()

	tick := time.NewTicker(every)
	defer tick.Stop()
	var last string
	for {
		// A failed poll is logged when its error changes, not at every tick.
		if err := w.Poll(ctx); err != nil {
			if err.Error() != last {
				log.Printf("vault-exporter: poll: %v", err)
				last = err.Error()
			}
		} else if last != "" {
			log.Printf("vault-exporter: polls succeed again")
			last = ""
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
