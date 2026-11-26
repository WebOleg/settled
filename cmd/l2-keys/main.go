// Command l2-keys makes sure the devnet L2 has one private key per operator role.
// Missing keys are created, existing keys are kept. It prints role addresses only.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/WebOleg/settled/internal/rolekeys"
)

func main() {
	dir := flag.String("dir", ".devnet/l2/keys", "directory that holds one key file per role")
	flag.Parse()

	entries, err := rolekeys.Ensure(*dir, rolekeys.All)
	if err != nil {
		fmt.Fprintf(os.Stderr, "l2-keys: FAIL: %v\n", err)
		os.Exit(1)
	}
	for _, e := range entries {
		fmt.Printf("%-20s %s\n", e.Role, e.Address)
	}
	fmt.Printf("l2-keys: ok, %d roles in %s (addresses in %s)\n", len(entries), *dir, rolekeys.AddressesFile)
}
