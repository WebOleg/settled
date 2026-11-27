// Command vault-deploy deploys the vault to the devnet L2 the way it would go to
// production. A TimelockController is the vault's admin, so every change of the rules
// waits in public before it applies; the vault runs behind an ERC-1967 proxy,
// initialized in the same transaction that creates the proxy, so no one can initialize
// it first. The first rule, the configuration of ETH, is itself proposed to the
// timelock and executed after its delay.
//
// The four keys (deployer, operator, guardian, timelock proposer) are created once in
// .devnet/vault/keys and topped up on the L2 by the devnet user (from
// SETTLED_DEVNET_MNEMONIC, never printed). The code comes from forge's build output, so
// what is deployed is what the repository builds; vault-verify checks it byte for byte.
// Running it again on the same L2 changes nothing.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/storagelayout"
	"github.com/WebOleg/settled/internal/timelockop"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/vaultconf"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// UserIndex is the devnet user, who holds ETH on the L2 and pays the keys' gas.
	UserIndex = 2
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "vault-deploy: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, rpcURL, outDir string) error {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read chain id: %w", err)
	}
	genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}

	// Once per L2: a record for this L2 with code at the vault means nothing to do; a
	// record for another L2 is refused rather than overwritten.
	existing, err := vaultconf.ReadDeployment(".")
	switch {
	case err == nil:
		if existing.L2Genesis != genesis.Hash() {
			return fmt.Errorf("%s/%s belongs to another L2 (genesis %s, this one %s); run make vault-reset", vaultconf.Dir, vaultconf.DeploymentFile, existing.L2Genesis, genesis.Hash())
		}
		code, err := client.CodeAt(ctx, existing.Vault, nil)
		if err != nil {
			return fmt.Errorf("read code at the vault: %w", err)
		}
		if len(code) == 0 {
			return fmt.Errorf("the recorded vault %s has no code on this L2; run make vault-reset", existing.Vault)
		}
		fmt.Printf("deployed         vault %s, timelock %s (block %d); nothing to do\n", existing.Vault, existing.Timelock, existing.Block)
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	config := vaultconf.NativeConfig()
	if err := config.Validate(); err != nil {
		return err
	}
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	layout, err := storagelayout.Parse(vault.StorageLayout)
	if err != nil {
		return fmt.Errorf("the vault's storage layout (foundry.toml extra_output): %w", err)
	}
	proxy, err := artifact.Load(outDir, "ERC1967Proxy.sol", "ERC1967Proxy")
	if err != nil {
		return err
	}
	timelock, err := artifact.Load(outDir, "TimelockController.sol", "TimelockController")
	if err != nil {
		return err
	}

	entries, err := rolekeys.Ensure(vaultconf.KeysDir, vaultconf.Roles)
	if err != nil {
		return err
	}
	addr := map[rolekeys.Role]common.Address{}
	for _, e := range entries {
		addr[e.Role] = e.Address
		fmt.Printf("key              %-18s %s\n", e.Role, e.Address)
	}
	deployer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.Deployer)
	if err != nil {
		return err
	}
	proposer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.TimelockProposer)
	if err != nil {
		return err
	}
	if err := fund(ctx, client, []common.Address{addr[vaultconf.Deployer], addr[vaultconf.TimelockProposer]}); err != nil {
		return err
	}

	delay := big.NewInt(vaultconf.TimelockDelay)
	proposers := []common.Address{addr[vaultconf.TimelockProposer]}
	// No separate admin: the timelock administers itself, so its own roles change only
	// through its delay.
	data, err := timelock.DeployData(delay, proposers, proposers, common.Address{})
	if err != nil {
		return err
	}
	receipt, timelockAddr, err := txsend.Deploy(ctx, client, deployer, data)
	if err != nil {
		return fmt.Errorf("deploy the timelock: %w", err)
	}
	fmt.Printf("timelock         %s in block %s, delay %d s\n", timelockAddr, receipt.BlockNumber, vaultconf.TimelockDelay)

	data, err = vault.DeployData()
	if err != nil {
		return err
	}
	receipt, implementation, err := txsend.Deploy(ctx, client, deployer, data)
	if err != nil {
		return fmt.Errorf("deploy the vault implementation: %w", err)
	}
	fmt.Printf("implementation   %s in block %s\n", implementation, receipt.BlockNumber)

	initialize, err := vault.Call("initialize", timelockAddr, big.NewInt(vaultconf.AdminDelay),
		addr[vaultconf.Operator], addr[vaultconf.Guardian], uint64(vaultconf.WithdrawalDelay))
	if err != nil {
		return err
	}
	data, err = proxy.DeployData(implementation, initialize)
	if err != nil {
		return err
	}
	receipt, vaultAddr, err := txsend.Deploy(ctx, client, deployer, data)
	if err != nil {
		return fmt.Errorf("deploy the proxy: %w", err)
	}
	fmt.Printf("vault            %s in block %s, initialized in the same transaction\n", vaultAddr, receipt.BlockNumber)

	// Recorded now: from here on the vault exists, and a rerun must not deploy another.
	deployment := &vaultconf.Deployment{
		ChainID: chainID.Uint64(), L2Genesis: genesis.Hash(), Timelock: timelockAddr,
		Implementation: implementation, Vault: vaultAddr, Block: receipt.BlockNumber.Uint64(),
		StorageLayout: layout,
	}
	if err := vaultconf.WriteDeployment(".", deployment); err != nil {
		return err
	}

	if err := configure(ctx, client, proposer, vault, timelock, timelockAddr, vaultAddr, config, delay); err != nil {
		return err
	}
	fmt.Printf("vault-deploy: ok, record in %s\n", filepath.Join(vaultconf.Dir, vaultconf.DeploymentFile))
	return nil
}

// fund tops up the keys that send transactions here, from the devnet user on the L2.
func fund(ctx context.Context, client *ethclient.Client, accounts []common.Address) error {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	user, err := devkeys.FromMnemonic(mnemonic, UserIndex)
	if err != nil {
		return fmt.Errorf("derive the devnet user key: %w", err)
	}
	target := new(big.Int).Div(funding.Ether(1), big.NewInt(50)) // 0.02 ETH
	targets := make([]funding.Target, len(accounts))
	for i, a := range accounts {
		targets[i] = funding.Target{Name: a.Hex(), Address: a, Balance: target}
	}
	results, err := funding.TopUp(ctx, client, user, targets)
	if err != nil {
		return fmt.Errorf("top up the keys from %s (make l2-deposit refills it): %w", crypto.PubkeyToAddress(user.PublicKey), err)
	}
	for _, r := range results {
		fmt.Printf("funded           %s with %s ETH (had %s)\n", r.Target.Address, funding.FormatEther(r.Sent), funding.FormatEther(r.Before))
	}
	return nil
}

// configure proposes the first token configuration to the timelock, waits out its
// delay, and executes it.
func configure(ctx context.Context, client *ethclient.Client, proposer *ecdsa.PrivateKey, vault, timelock *artifact.Artifact,
	timelockAddr, vaultAddr common.Address, config vaultconf.TokenConfig, delay *big.Int,
) error {
	call, err := vault.Call("setTokenConfig", vaultconf.Native, config)
	if err != nil {
		return err
	}
	op := timelockop.Op{Target: vaultAddr, Data: call, Salt: crypto.Keccak256Hash([]byte("settled: vault initial token config"))}
	receipt, _, err := timelockop.Run(ctx, client, proposer, timelock, timelockAddr, op, delay, func(s string) {
		fmt.Printf("proposed         ETH configuration: %s\n", s)
	})
	if err != nil {
		return fmt.Errorf("configure ETH through the timelock: %w", err)
	}
	fmt.Printf("configured       ETH: capacity %s ETH per %d blocks, queued from %s ETH; block %s\n",
		funding.FormatEther(config.Capacity), config.RefillPeriod, funding.FormatEther(config.LargeThreshold), receipt.BlockNumber)
	return nil
}
