// Command l2-peer-id prints the libp2p peer ID of an op-node peer-to-peer key file. The
// file must be readable by its owner only; the key itself is never printed.
package main

import (
	"fmt"
	"os"

	"github.com/WebOleg/settled/internal/p2pid"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: l2-peer-id <key file>")
		os.Exit(2)
	}
	path := os.Args[1]
	info, err := os.Stat(path)
	if err != nil {
		fail(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		fail(fmt.Errorf("%s has mode %o, want 600", path, info.Mode().Perm()))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	id, err := p2pid.FromPrivateKeyHex(string(raw))
	if err != nil {
		fail(err)
	}
	fmt.Println(id)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "l2-peer-id: FAIL: %v\n", err)
	os.Exit(1)
}
