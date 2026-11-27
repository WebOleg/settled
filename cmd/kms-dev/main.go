// Command kms-dev is the devnet's key management service (internal/kmsdev): the part of
// the AWS KMS API the withdrawal signer uses, with secp256k1 keys that never leave it.
// Three principals reach it, each with credentials of its own read from a file: the
// admin, who makes keys and names them (make vault-payer); the signer, which may only
// read the public key of alias/payer and sign with it; and key rotation, which may only
// do the same with alias/payer-retiring, the key a rotation replaced, to move its funds
// to the new one. Keys are kept in -dir, a volume of
// its own. It is on the bridge's network, and published on 127.0.0.1 only, for the
// admin. It counts its calls for Prometheus on a port of its own, a denied one above all:
// a caller that is not who it says, or asks what its policy does not allow.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/WebOleg/settled/internal/awscreds"
	"github.com/WebOleg/settled/internal/kmsdev"
)

func main() {
	listen := flag.String("listen", ":4599", "address to serve the KMS API on")
	metricsAddr := flag.String("metrics", ":9113", "address to serve Prometheus metrics on")
	dir := flag.String("dir", "/data", "folder the keys are kept in, readable by the service only")
	region := flag.String("region", "eu-central-1", "the region the service answers for")
	account := flag.String("account", "000000000000", "the account in its ARNs")
	adminFile := flag.String("admin-credentials", "/run/secrets/kms_admin", "file holding the admin's access key id and secret")
	signerFile := flag.String("signer-credentials", "/run/secrets/kms_signer", "file holding the signer's access key id and secret")
	rotationFile := flag.String("rotation-credentials", "/run/secrets/kms_rotation", "file holding key rotation's access key id and secret")
	flag.Parse()

	adminID, adminSecret, err := awscreds.Read(*adminFile)
	if err != nil {
		log.Fatalf("kms-dev: %v", err)
	}
	signerID, signerSecret, err := awscreds.Read(*signerFile)
	if err != nil {
		log.Fatalf("kms-dev: %v", err)
	}
	rotationID, rotationSecret, err := awscreds.Read(*rotationFile)
	if err != nil {
		log.Fatalf("kms-dev: %v", err)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatalf("kms-dev: %v", err)
	}
	if err := os.Chmod(*dir, 0o700); err != nil {
		log.Fatalf("kms-dev: %v", err)
	}
	srv := &kmsdev.Server{Region: *region, Account: *account, Dir: *dir, Principals: []kmsdev.Principal{
		{Name: "admin", AccessKeyID: adminID, SecretAccessKey: adminSecret,
			Actions: []string{"kms:CreateKey", "kms:CreateAlias", "kms:UpdateAlias", "kms:DescribeKey", "kms:GetPublicKey"}},
		{Name: "withdrawal-signer", AccessKeyID: signerID, SecretAccessKey: signerSecret,
			Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer"}},
		{Name: "key-rotation", AccessKeyID: rotationID, SecretAccessKey: rotationSecret,
			Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer-retiring"}},
	}}
	m := newMetrics([]string{"admin", "withdrawal-signer", "key-rotation"})
	srv.Observe = m.observe
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := m.write(rw); err != nil {
			log.Printf("kms-dev: write metrics: %v", err)
		}
	})
	mux.HandleFunc("/-/healthy", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write([]byte("ok\n")) })
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second}
	metrics := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 2)
	go func() { served <- server.ListenAndServe() }()
	go func() { served <- metrics.ListenAndServe() }()
	log.Printf("kms-dev: serving %s for %s; keys in %s", *listen, *region, *dir)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := errors.Join(server.Shutdown(shutdown), metrics.Shutdown(shutdown)); err != nil {
			log.Fatalf("kms-dev: %v", err)
		}
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("kms-dev: %v", err)
		}
	}
}
