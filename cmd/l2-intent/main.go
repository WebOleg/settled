// Command l2-intent writes the op-deployer intent file for the devnet L2 from the role
// addresses that make l2-keys produced. It reads no private key.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/WebOleg/settled/internal/l2intent"
	"github.com/WebOleg/settled/internal/rolekeys"
)

func main() {
	addresses := flag.String("addresses", ".devnet/l2/keys/"+rolekeys.AddressesFile, "role addresses written by make l2-keys")
	version := flag.String("op-deployer-version", "", "version string printed by op-deployer --version")
	l1 := flag.Uint64("l1-chain-id", 3151908, "L1 chain id")
	l2 := flag.Uint64("l2-chain-id", 420420, "L2 chain id")
	out := flag.String("out", "", "file to write the intent to")
	flag.Parse()

	if err := run(*addresses, *version, *l1, *l2, *out); err != nil {
		fmt.Fprintf(os.Stderr, "l2-intent: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(addresses, version string, l1, l2 uint64, out string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}
	raw, err := os.ReadFile(addresses)
	if err != nil {
		return fmt.Errorf("read %s (run make l2-keys first): %w", addresses, err)
	}
	var entries []rolekeys.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("parse %s: %w", addresses, err)
	}
	roles := make(map[rolekeys.Role]common.Address, len(entries))
	for _, e := range entries {
		roles[e.Role] = e.Address
	}

	intent, err := l2intent.Render(l2intent.Params{
		L1ChainID: l1, L2ChainID: l2, OpDeployerVersion: version, Roles: roles,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, intent, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Printf("l2-intent: wrote %s for L1 %d, L2 %d\n", out, l1, l2)
	return nil
}
