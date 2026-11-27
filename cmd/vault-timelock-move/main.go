// Command vault-timelock-move moves the vault's admin to a new timelock, the way it is
// done in production: nothing is redeployed and the vault's state stays where it is.
// The new timelock has its roles split (internal/timelockop): the proposer proposes and
// executes, a separate canceller cancels. The move goes through both delays:
//
//  1. the deployer deploys the new timelock;
//  2. the old timelock schedules beginDefaultAdminTransfer(new) on the vault and, after
//     its delay, executes it, which starts the vault's admin handover delay;
//  3. the new timelock schedules acceptDefaultAdminTransfer() so that it is ready just
//     as the handover delay passes, and executes it.
//
// The record names the new timelock as soon as it exists (nextTimelock), so a run that
// stops midway goes on from where it stopped. When the timelock the vault answers to
// already has its roles split, there is nothing to do.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/timelockop"
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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "vault-timelock-move: FAIL: %v\n", err)
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
		return errors.New("the deployment record belongs to another L2; run make vault-reset and make vault-deploy")
	}
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
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
	}
	proposerAddr, cancellerAddr := addr[vaultconf.TimelockProposer], addr[vaultconf.TimelockCanceller]
	fmt.Printf("key              timelock-canceller %s\n", cancellerAddr)

	admin, err := address(ctx, client, vault, d.Vault, "defaultAdmin")
	if err != nil {
		return err
	}
	if admin != d.Timelock && (d.NextTimelock == nil || admin != *d.NextTimelock) {
		return fmt.Errorf("the vault's admin is %s, neither the recorded timelock %s nor the one being moved to", admin, d.Timelock)
	}
	if admin == d.Timelock && d.NextTimelock == nil {
		split, err := splitRoles(ctx, client, timelock, d.Timelock, proposerAddr, cancellerAddr)
		if err != nil {
			return err
		}
		if split {
			fmt.Printf("up to date       the vault's admin %s already has its roles split; nothing to do\n", d.Timelock)
			return nil
		}
	}

	deployer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.Deployer)
	if err != nil {
		return err
	}
	proposer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.TimelockProposer)
	if err != nil {
		return err
	}
	if err := fund(ctx, client, []common.Address{addr[vaultconf.Deployer], proposerAddr}); err != nil {
		return err
	}
	delay := big.NewInt(vaultconf.TimelockDelay)

	// 1. The new timelock, recorded at once so a stopped run goes on with it.
	if d.NextTimelock == nil {
		next, receipt, err := timelockop.Deploy(ctx, client, deployer, timelock, delay, proposerAddr, cancellerAddr)
		if err != nil {
			return fmt.Errorf("deploy the new timelock: %w", err)
		}
		d.NextTimelock = &next
		if err := vaultconf.WriteDeployment(".", d); err != nil {
			return err
		}
		fmt.Printf("new timelock     %s in block %s; the proposer proposes and executes, the canceller cancels\n", next, receipt.BlockNumber)
	} else {
		fmt.Printf("new timelock     %s, from the record\n", *d.NextTimelock)
	}
	next := *d.NextTimelock

	if admin != next {
		// 2. The old timelock hands the admin role over.
		pending, _, err := pendingAdmin(ctx, client, vault, d.Vault)
		if err != nil {
			return err
		}
		if pending != next {
			call, err := vault.Call("beginDefaultAdminTransfer", next)
			if err != nil {
				return err
			}
			op := timelockop.Op{Target: d.Vault, Data: call, Salt: crypto.Keccak256Hash([]byte("settled: vault admin to " + next.Hex()))}
			if _, _, err := timelockop.Run(ctx, client, proposer, timelock, d.Timelock, op, delay, func(s string) {
				fmt.Printf("old timelock     beginDefaultAdminTransfer: %s\n", s)
			}); err != nil {
				return fmt.Errorf("begin the admin handover through the old timelock: %w", err)
			}
		}
		pending, schedule, err := pendingAdmin(ctx, client, vault, d.Vault)
		if err != nil {
			return err
		}
		if pending != next {
			return fmt.Errorf("the vault's pending admin is %s, want %s", pending, next)
		}
		fmt.Printf("handover         pending to %s, acceptable after %s\n", next, time.Unix(int64(schedule), 0).UTC().Format(time.RFC3339))

		// 3. The new timelock accepts. Its own delay runs while the handover delay ends:
		// it is scheduled so that it is ready just after the handover may be accepted.
		if err := waitForTime(ctx, client, schedule-uint64(vaultconf.TimelockDelay)+2); err != nil {
			return err
		}
		accept, err := vault.Call("acceptDefaultAdminTransfer")
		if err != nil {
			return err
		}
		op := timelockop.Op{Target: d.Vault, Data: accept, Salt: crypto.Keccak256Hash([]byte("settled: vault admin accepted by " + next.Hex()))}
		if _, _, err := timelockop.Run(ctx, client, proposer, timelock, next, op, delay, func(s string) {
			fmt.Printf("new timelock     acceptDefaultAdminTransfer: %s\n", s)
		}); err != nil {
			return fmt.Errorf("accept the admin handover through the new timelock: %w", err)
		}
	}

	admin, err = address(ctx, client, vault, d.Vault, "defaultAdmin")
	if err != nil {
		return err
	}
	if admin != next {
		return fmt.Errorf("after the move the vault's admin is %s, want %s", admin, next)
	}
	previous := d.Timelock
	d.Timelock = next
	d.NextTimelock = nil
	if err := vaultconf.WriteDeployment(".", d); err != nil {
		return err
	}
	fmt.Printf("moved            the vault's admin is %s; the previous timelock %s holds no role on it\n", next, previous)
	fmt.Println("vault-timelock-move: ok")
	return nil
}

// splitRoles reports whether the timelock at `at` has the proposer without the
// canceller role and the canceller with it.
func splitRoles(ctx context.Context, client *ethclient.Client, timelock *artifact.Artifact, at, proposer, canceller common.Address) (bool, error) {
	proposerCancels, err := timelockop.HasRole(ctx, client, timelock, at, timelockop.CancellerRole, proposer)
	if err != nil {
		return false, err
	}
	cancellerCancels, err := timelockop.HasRole(ctx, client, timelock, at, timelockop.CancellerRole, canceller)
	if err != nil {
		return false, err
	}
	return !proposerCancels && cancellerCancels, nil
}

// address calls a view of the vault that returns one address.
func address(ctx context.Context, client *ethclient.Client, vault *artifact.Artifact, at common.Address, method string) (common.Address, error) {
	out, err := callView(ctx, client, vault, at, method)
	if err != nil {
		return common.Address{}, err
	}
	a, ok := out[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("%s(): not an address", method)
	}
	return a, nil
}

// pendingAdmin reads pendingDefaultAdmin(): the pending admin and when it may accept.
func pendingAdmin(ctx context.Context, client *ethclient.Client, vault *artifact.Artifact, at common.Address) (common.Address, uint64, error) {
	out, err := callView(ctx, client, vault, at, "pendingDefaultAdmin")
	if err != nil {
		return common.Address{}, 0, err
	}
	if len(out) != 2 {
		return common.Address{}, 0, fmt.Errorf("pendingDefaultAdmin(): %d values, want 2", len(out))
	}
	a, ok := out[0].(common.Address)
	s, ok2 := out[1].(*big.Int)
	if !ok || !ok2 {
		return common.Address{}, 0, errors.New("pendingDefaultAdmin(): unexpected types")
	}
	return a, s.Uint64(), nil
}

func callView(ctx context.Context, client *ethclient.Client, a *artifact.Artifact, at common.Address, method string) ([]any, error) {
	data, err := a.Call(method)
	if err != nil {
		return nil, err
	}
	raw, err := client.CallContract(ctx, ethereum.CallMsg{To: &at, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s(): %w", method, err)
	}
	out, err := a.ABI.Unpack(method, raw)
	if err != nil || len(out) == 0 {
		return nil, fmt.Errorf("decode %s(): %w", method, errors.Join(err, errors.New("no value")))
	}
	return out, nil
}

// waitForTime waits until the latest L2 block's timestamp reaches ts.
func waitForTime(ctx context.Context, client *ethclient.Client, ts uint64) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			return fmt.Errorf("read the head: %w", err)
		}
		if head.Time >= ts {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("time %d not reached: %w", ts, ctx.Err())
		case <-tick.C:
		}
	}
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
	return topUp(ctx, client, user, accounts)
}

func topUp(ctx context.Context, client *ethclient.Client, user *ecdsa.PrivateKey, accounts []common.Address) error {
	target := new(big.Int).Div(funding.Ether(1), big.NewInt(50)) // 0.02 ETH
	targets := make([]funding.Target, len(accounts))
	for i, a := range accounts {
		targets[i] = funding.Target{Name: a.Hex(), Address: a, Balance: target}
	}
	if _, err := funding.TopUp(ctx, client, user, targets); err != nil {
		return fmt.Errorf("top up the keys from %s (make l2-deposit refills it): %w", crypto.PubkeyToAddress(user.PublicKey), err)
	}
	return nil
}
