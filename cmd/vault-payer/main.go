// Command vault-payer gives the withdrawal payer a key of its own on the devnet, the way
// a role is given in production: through the timelock that administers the vault.
//
//  1. the payer's key is made, once, with the vault's other keys (internal/rolekeys);
//  2. if it does not hold the vault's operator role, the timelock proposer schedules
//     grantRole(OPERATOR_ROLE, payer) and, after the timelock's delay, executes it;
//  3. the key is topped up with ETH for gas on the L2, from the devnet user;
//  4. the payer holds the operator role and nothing else.
//
// With -secret-only it only makes the key and writes it where the withdrawal signer's
// container reads it (.devnet/bridge/signing-key.txt), with no chain needed; the payer
// itself never holds it. Run again, it changes
// nothing that is in place. No key is ever printed, only addresses.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/timelockop"
	"github.com/WebOleg/settled/internal/vaultconf"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// userIndex is the devnet user, who holds ETH on the L2 and pays the keys' gas.
	userIndex = 2
	// SecretFile is where the withdrawal signer's container reads the key, as a Docker
	// secret. The folder is readable by its owner only (scripts/bridge/bridge-init.sh);
	// the file is readable by the container's own user, as the bridge's passwords are.
	SecretFile = ".devnet/bridge/signing-key.txt"
	// formerSecretFile is where the payer's container read it before the signer held it.
	formerSecretFile = ".devnet/bridge/payer-key.txt"
)

// gas is what the payer's key is topped up to: a few hundred withdrawals on the devnet.
var gas = new(big.Int).Div(funding.Ether(1), big.NewInt(20)) // 0.05 ETH

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	secretOnly := flag.Bool("secret-only", false, "only make the key and write it for the signer's container")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir, *secretOnly); err != nil {
		fmt.Fprintf(os.Stderr, "vault-payer: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("vault-payer: ok")
}

func run(ctx context.Context, rpcURL, outDir string, secretOnly bool) error {
	entries, err := rolekeys.Ensure(vaultconf.KeysDir, vaultconf.Roles)
	if err != nil {
		return err
	}
	addr := map[rolekeys.Role]common.Address{}
	for _, e := range entries {
		addr[e.Role] = e.Address
	}
	payer := addr[vaultconf.Payer]
	key, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.Payer)
	if err != nil {
		return err
	}
	// The bridge's folder is made by make bridge-up, which writes the secret itself; a
	// vault deployed before it has none to write to yet.
	if _, err := os.Stat(filepath.Dir(SecretFile)); err == nil || secretOnly {
		written, err := writeSecret(SecretFile, key)
		if err != nil {
			return err
		}
		if written {
			fmt.Printf("key              payer %s, written for the signer's container\n", payer)
		} else {
			fmt.Printf("key              payer %s, in place for the signer's container\n", payer)
		}
		// No copy of the key stays where the payer could read it.
		if err := os.Remove(formerSecretFile); err == nil {
			fmt.Printf("key              removed the copy the payer read before the signer held the key\n")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		fmt.Printf("key              payer %s; make bridge-up writes it for the signer's container\n", payer)
	}
	if secretOnly {
		return nil
	}

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
		return errors.New("the deployment record belongs to another L2; run make vault-reset and make vault-deploy")
	}
	if d.NextTimelock != nil {
		return errors.New("a move to another timelock is half done; run make vault-timelock-move first")
	}

	holds, err := hasRole(ctx, client, d.Vault, vaultconf.OperatorRole, payer)
	if err != nil {
		return err
	}
	user, err := devUser()
	if err != nil {
		return err
	}
	if !holds {
		timelock, err := artifact.Load(outDir, "TimelockController.sol", "TimelockController")
		if err != nil {
			return err
		}
		vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
		if err != nil {
			return err
		}
		proposer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.TimelockProposer)
		if err != nil {
			return err
		}
		if err := topUp(ctx, client, user, addr[vaultconf.TimelockProposer], new(big.Int).Div(funding.Ether(1), big.NewInt(50))); err != nil {
			return err
		}
		grant, err := vault.Call("grantRole", vaultconf.OperatorRole, payer)
		if err != nil {
			return err
		}
		op := timelockop.Op{Target: d.Vault, Data: grant, Salt: crypto.Keccak256Hash([]byte("settled: operator role to the payer " + payer.Hex()))}
		if _, _, err := timelockop.Run(ctx, client, proposer, timelock, d.Timelock, op, big.NewInt(vaultconf.TimelockDelay), func(s string) {
			fmt.Printf("timelock         grantRole(OPERATOR_ROLE, payer): %s\n", s)
		}); err != nil {
			return fmt.Errorf("grant the payer the operator role through the timelock: %w", err)
		}
	}
	if err := topUp(ctx, client, user, payer, gas); err != nil {
		return err
	}

	// The payer holds the operator role, and only that.
	for _, r := range []struct {
		name string
		role common.Hash
		want bool
	}{{"OPERATOR_ROLE", vaultconf.OperatorRole, true}, {"GUARDIAN_ROLE", vaultconf.GuardianRole, false}, {"DEFAULT_ADMIN_ROLE", vaultconf.AdminRole, false}} {
		got, err := hasRole(ctx, client, d.Vault, r.role, payer)
		if err != nil {
			return err
		}
		if got != r.want {
			return fmt.Errorf("the payer %s: hasRole(%s) is %t, want %t", payer, r.name, got, r.want)
		}
	}
	balance, err := client.BalanceAt(ctx, payer, nil)
	if err != nil {
		return err
	}
	fmt.Printf("payer            %s holds OPERATOR_ROLE and nothing else; %s ETH for gas\n", payer, funding.FormatEther(balance))
	return nil
}

// writeSecret writes the key for the signer's container, when it is not there already,
// through a temporary file and a rename. It reports whether it wrote.
func writeSecret(path string, key *ecdsa.PrivateKey) (bool, error) {
	want := []byte(hex.EncodeToString(crypto.FromECDSA(key)) + "\n")
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("%s: %w (make bridge-up makes it)", dir, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("%w: %s has mode %o, want 700", rolekeys.ErrInsecurePermissions, dir, info.Mode().Perm())
	}
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, want) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return false, fmt.Errorf("write the key for the signer's container: %w", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("write the key for the signer's container: %w", err)
	}
	return true, nil
}

func hasRole(ctx context.Context, client *ethclient.Client, vault common.Address, role common.Hash, account common.Address) (bool, error) {
	words, err := chaincall.Words(ctx, client, vault, "hasRole(bytes32,address)", role, chaincall.AddressWord(account))
	if err != nil {
		return false, err
	}
	if len(words) != 1 {
		return false, fmt.Errorf("hasRole: %d words, want 1", len(words))
	}
	return words[0] == common.BigToHash(big.NewInt(1)), nil
}

func devUser() (*ecdsa.PrivateKey, error) {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return nil, fmt.Errorf("%s is not set", mnemonicEnv)
	}
	user, err := devkeys.FromMnemonic(mnemonic, userIndex)
	if err != nil {
		return nil, fmt.Errorf("derive the devnet user key: %w", err)
	}
	return user, nil
}

func topUp(ctx context.Context, client *ethclient.Client, user *ecdsa.PrivateKey, account common.Address, balance *big.Int) error {
	if _, err := funding.TopUp(ctx, client, user, []funding.Target{{Name: account.Hex(), Address: account, Balance: balance}}); err != nil {
		return fmt.Errorf("top up %s from %s (make l2-deposit refills it): %w", account, crypto.PubkeyToAddress(user.PublicKey), err)
	}
	return nil
}
