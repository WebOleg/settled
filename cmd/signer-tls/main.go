// Command signer-tls makes the devnet's TLS material for the withdrawal signer, in
// .devnet/bridge/tls: a CA of its own, the signer's server certificate and the payer's
// client certificate (internal/signer). It makes them when one is missing, does not
// chain to the CA, or ends within 30 days, and otherwise changes nothing. No key is
// ever printed.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/WebOleg/settled/internal/signer"
)

func main() {
	dir := flag.String("dir", ".devnet/bridge/tls", "the folder, made readable by its owner only")
	flag.Parse()
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "signer-tls: FAIL: %v\n", err)
		os.Exit(1)
	}
	wrote, err := signer.EnsureTLS(*dir, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "signer-tls: FAIL: %v\n", err)
		os.Exit(1)
	}
	if wrote {
		fmt.Println("signer-tls: made the CA, the signer's and the payer's certificates")
	}
	fmt.Println("signer-tls: ok")
}
