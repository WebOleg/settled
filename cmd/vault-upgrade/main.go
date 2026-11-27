// Command vault-upgrade moves the vault on the devnet L2 to the implementation the
// repository builds now, the way an upgrade goes in production: the deployer deploys the
// new implementation, the timelock proposer schedules upgradeToAndCall on the vault
// through the timelock, and after its delay the timelock executes it. The vault's state
// stays where it is, in the proxy; vault-verify then checks the code, the roles and the
// configuration again. If the vault already runs the code the repository builds, it does
// nothing.
//
// Before anything is proposed, the storage layout of the new implementation is checked
// against the layout recorded for the one the vault runs: every variable must stay where
// it is, with its name and type, and nothing new may share its bytes. A record without a
// layout gets one only when the vault runs exactly the code the repository builds, so the
// layout recorded is the layout of the code deployed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/bytecode"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/storagelayout"
	"github.com/WebOleg/settled/internal/timelockop"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/vaultconf"
)

// implementationSlot is the ERC-1967 slot of a proxy's implementation.
var implementationSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "vault-upgrade: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, rpcURL, outDir string) error {
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return fmt.Errorf("%w (run make vault-deploy)", err)
	}
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return fmt.Errorf("the deployment record belongs to another L2; run make vault-reset and make vault-deploy")
	}

	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	timelock, err := artifact.Load(outDir, "TimelockController.sol", "TimelockController")
	if err != nil {
		return err
	}
	current, err := implementation(ctx, client, d.Vault)
	if err != nil {
		return err
	}
	if current != d.Implementation {
		return fmt.Errorf("the vault points at %s, the record says %s", current, d.Implementation)
	}
	code, err := client.CodeAt(ctx, current, nil)
	if err != nil {
		return fmt.Errorf("read the implementation's code: %w", err)
	}
	layout, err := storagelayout.Parse(vault.StorageLayout)
	if err != nil {
		return fmt.Errorf("the build's storage layout (foundry.toml extra_output): %w", err)
	}
	if _, err := bytecode.Compare(vault.Runtime, code, vault.Immutables); err == nil {
		if d.StorageLayout == nil {
			d.StorageLayout = layout
			if err := vaultconf.WriteDeployment(".", d); err != nil {
				return err
			}
			fmt.Printf("layout recorded  %d variables, from the build the vault runs\n", len(layout))
		}
		fmt.Printf("up to date       the vault at %s runs the code the repository builds (%s); nothing to do\n", d.Vault, current)
		return nil
	}
	fmt.Printf("current          %s differs from the build\n", current)
	if d.StorageLayout == nil {
		return errors.New("no storage layout is recorded for the code the vault runs, so the upgrade cannot be checked; record it from a build of that code first")
	}
	if err := storagelayout.Compatible(d.StorageLayout, layout); err != nil {
		return fmt.Errorf("the new implementation would move the vault's state:\n%w", err)
	}
	fmt.Printf("storage layout   compatible: the %d recorded variables keep slot, offset, name and type\n", len(d.StorageLayout))

	deployer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.Deployer)
	if err != nil {
		return err
	}
	proposer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.TimelockProposer)
	if err != nil {
		return err
	}
	data, err := vault.DeployData()
	if err != nil {
		return err
	}
	receipt, next, err := txsend.Deploy(ctx, client, deployer, data)
	if err != nil {
		return fmt.Errorf("deploy the new implementation: %w", err)
	}
	fmt.Printf("implementation   %s in block %s\n", next, receipt.BlockNumber)

	call, err := vault.Call("upgradeToAndCall", next, []byte{})
	if err != nil {
		return err
	}
	op := timelockop.Op{Target: d.Vault, Data: call, Salt: crypto.Keccak256Hash([]byte("settled: vault upgrade to " + next.Hex()))}
	receipt, _, err = timelockop.Run(ctx, client, proposer, timelock, d.Timelock, op, big.NewInt(vaultconf.TimelockDelay), func(s string) {
		fmt.Printf("proposed         upgrade: %s\n", s)
	})
	if err != nil {
		return fmt.Errorf("upgrade through the timelock: %w", err)
	}
	now, err := implementation(ctx, client, d.Vault)
	if err != nil {
		return err
	}
	if now != next {
		return fmt.Errorf("after the upgrade the vault points at %s, want %s", now, next)
	}
	d.Implementation = next
	d.StorageLayout = layout
	if err := vaultconf.WriteDeployment(".", d); err != nil {
		return err
	}
	fmt.Printf("upgraded         vault %s now runs %s (block %s); previous %s\n", d.Vault, next, receipt.BlockNumber, current)
	fmt.Println("vault-upgrade: ok")
	return nil
}

func implementation(ctx context.Context, client *ethclient.Client, proxy common.Address) (common.Address, error) {
	slot, err := client.StorageAt(ctx, proxy, implementationSlot, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("read the implementation slot: %w", err)
	}
	return common.BytesToAddress(slot), nil
}
