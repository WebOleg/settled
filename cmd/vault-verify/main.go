// Command vault-verify checks the vault on the devnet L2 against the repository and its
// deployment record. It needs no key. It checks:
//
//   - the code: the timelock, the proxy and the implementation are byte for byte what
//     forge builds from this repository, immutables aside, and the implementation's one
//     immutable is its own address;
//   - the proxy points at that implementation, and the implementation cannot be
//     initialized directly;
//   - the roles: the timelock is the vault's admin, with nothing pending; the operator
//     and the guardian hold their roles and nothing else; the deployer and the timelock
//     proposer hold no role on the vault; the timelock administers itself;
//   - the parameters: every delay and the ETH configuration are the ones in
//     internal/vaultconf, and the vault is not paused;
//   - the vault's clock: it counts L2 blocks (ERC-6372), and the L2 makes a block every
//     2 s, fixed by derivation, so the delays in blocks are the durations intended.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/bytecode"
	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/vaultconf"
)

// implementationSlot is the ERC-1967 slot of a proxy's implementation:
// keccak256("eip1967.proxy.implementation") - 1.
var implementationSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")

// invalidInitialization is the selector of Initializable.InvalidInitialization().
const invalidInitialization = "f92ee8a9"

type verifier struct {
	ctx    context.Context
	client *ethclient.Client
	total  int
	failed int
}

func (v *verifier) report(name string, err error, detail string) {
	v.total++
	if err != nil {
		v.failed++
		fmt.Printf("FAIL  %-52s %v\n", name, err)
		return
	}
	fmt.Printf("ok    %-52s %s\n", name, detail)
}

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	failed, total, err := run(ctx, *rpcURL, *outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vault-verify: FAIL: %v\n", err)
		os.Exit(1)
	}
	if failed > 0 {
		fmt.Printf("vault-verify: FAIL, %d of %d checks\n", failed, total)
		os.Exit(1)
	}
	fmt.Printf("vault-verify: ok, %d checks\n", total)
}

func run(ctx context.Context, rpcURL, outDir string) (int, int, error) {
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return 0, 0, fmt.Errorf("%w (run make vault-deploy)", err)
	}
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return 0, 0, fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	roles, err := addresses()
	if err != nil {
		return 0, 0, err
	}
	v := &verifier{ctx: ctx, client: client}

	genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return 0, 0, fmt.Errorf("read the L2 genesis: %w", err)
	}
	v.report("the record is for this L2", equal(genesis.Hash(), d.L2Genesis), d.L2Genesis.Hex())
	v.report(fmt.Sprintf("L2 makes a block every %d s, by derivation", vaultconf.L2BlockTime), fixedBlockTime(ctx, client, genesis.Time), "timestamp = genesis + block time x number")

	// Code.
	for _, c := range []struct {
		name, source, contract string
		at                     common.Address
		immutable              []byte // expected value of every immutable, if any
	}{
		{"timelock code is the build of TimelockController", "TimelockController.sol", "TimelockController", d.Timelock, nil},
		{"proxy code is the build of ERC1967Proxy", "ERC1967Proxy.sol", "ERC1967Proxy", d.Vault, nil},
		{"implementation code is the build of Vault", "Vault.sol", "Vault", d.Implementation, common.LeftPadBytes(d.Implementation.Bytes(), 32)},
	} {
		v.report(c.name, v.code(outDir, c.source, c.contract, c.at, c.immutable), c.at.Hex())
	}
	slot, err := client.StorageAt(ctx, d.Vault, implementationSlot, nil)
	if err == nil {
		err = equal(common.BytesToAddress(slot), d.Implementation)
	}
	v.report("proxy implementation slot (ERC-1967)", err, d.Implementation.Hex())
	v.report("implementation refuses initialize", v.initializeRefused(outDir, d.Implementation, roles), "InvalidInitialization")

	// Roles on the vault.
	v.address("vault admin is the timelock", d.Vault, "defaultAdmin()", d.Timelock)
	pending, err := chaincall.Words(ctx, client, d.Vault, "pendingDefaultAdmin()")
	if err == nil && (len(pending) != 2 || pending[0] != (common.Hash{}) || pending[1] != (common.Hash{})) {
		err = fmt.Errorf("an admin handover is pending: %x", pending)
	}
	v.report("no admin handover pending", err, "none")
	v.number("admin handover delay", d.Vault, "defaultAdminDelay()", vaultconf.AdminDelay)
	v.role("operator holds OPERATOR_ROLE", d.Vault, vaultconf.OperatorRole, roles[vaultconf.Operator], true)
	v.role("guardian holds GUARDIAN_ROLE", d.Vault, vaultconf.GuardianRole, roles[vaultconf.Guardian], true)
	for _, r := range []struct {
		role rolekeys.Role
		name string
	}{{vaultconf.Operator, "operator"}, {vaultconf.Guardian, "guardian"}, {vaultconf.Deployer, "deployer"}, {vaultconf.TimelockProposer, "timelock proposer"}} {
		for _, h := range []struct {
			hash common.Hash
			name string
		}{{vaultconf.OperatorRole, "OPERATOR_ROLE"}, {vaultconf.GuardianRole, "GUARDIAN_ROLE"}, {vaultconf.AdminRole, "DEFAULT_ADMIN_ROLE"}} {
			if (r.role == vaultconf.Operator && h.name == "OPERATOR_ROLE") || (r.role == vaultconf.Guardian && h.name == "GUARDIAN_ROLE") {
				continue
			}
			v.role(fmt.Sprintf("%s lacks %s on the vault", r.name, h.name), d.Vault, h.hash, roles[r.role], false)
		}
	}

	// The timelock.
	v.number("timelock delay", d.Timelock, "getMinDelay()", vaultconf.TimelockDelay)
	for _, h := range []struct {
		hash common.Hash
		name string
	}{{vaultconf.ProposerRole, "PROPOSER_ROLE"}, {vaultconf.ExecutorRole, "EXECUTOR_ROLE"}, {vaultconf.CancellerRole, "CANCELLER_ROLE"}} {
		v.role("timelock proposer holds "+h.name, d.Timelock, h.hash, roles[vaultconf.TimelockProposer], true)
	}
	v.role("timelock administers itself", d.Timelock, vaultconf.AdminRole, d.Timelock, true)
	v.role("deployer lacks the timelock admin role", d.Timelock, vaultconf.AdminRole, roles[vaultconf.Deployer], false)
	v.role("deployer lacks PROPOSER_ROLE", d.Timelock, vaultconf.ProposerRole, roles[vaultconf.Deployer], false)

	// Parameters.
	v.number("withdrawal queue delay, in blocks", d.Vault, "withdrawalDelay()", vaultconf.WithdrawalDelay)
	v.report("vault clock mode is the block number", v.clockMode(outDir, d.Vault), vaultconf.ClockMode)
	n, err := v.clock(outDir, d.Vault)
	v.report("vault clock reads the L2 block number", err, fmt.Sprintf("block %d", n))
	v.number("not paused", d.Vault, "paused()", 0)
	v.tokenConfig(d.Vault)
	return v.failed, v.total, nil
}

// fixedBlockTime checks what the vault's clock relies on for its delays to be durations:
// the rollup's block time is the one vaultconf assumes, and the latest L2 block's
// timestamp is the genesis timestamp plus that block time per block, so blocks come at
// that pace and no one chooses it.
func fixedBlockTime(ctx context.Context, client *ethclient.Client, genesisTime uint64) error {
	raw, err := os.ReadFile(filepath.Join(".devnet", "l2", "deployer", "rollup.json"))
	if err != nil {
		return fmt.Errorf("read rollup.json: %w", err)
	}
	var rollup struct {
		BlockTime uint64 `json:"block_time"`
	}
	if err := json.Unmarshal(raw, &rollup); err != nil || rollup.BlockTime == 0 {
		return fmt.Errorf("no block_time in rollup.json: %w", errors.Join(err, errors.New("missing")))
	}
	if rollup.BlockTime != vaultconf.L2BlockTime {
		return fmt.Errorf("rollup block time is %d s, the vault's delays assume %d s", rollup.BlockTime, vaultconf.L2BlockTime)
	}
	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("read the latest header: %w", err)
	}
	want := genesisTime + rollup.BlockTime*head.Number.Uint64()
	if head.Time != want {
		return fmt.Errorf("block %s has timestamp %d, want %d", head.Number, head.Time, want)
	}
	return nil
}

// clockMode reads the vault's CLOCK_MODE().
func (v *verifier) clockMode(outDir string, vault common.Address) error {
	out, err := v.callVault(outDir, vault, "CLOCK_MODE", nil)
	if err != nil {
		return err
	}
	if mode, ok := out[0].(string); !ok || mode != vaultconf.ClockMode {
		return fmt.Errorf("CLOCK_MODE() is %v, want %q", out[0], vaultconf.ClockMode)
	}
	return nil
}

// clock reads the vault's clock() in the latest block and checks that it is that block's
// number.
func (v *verifier) clock(outDir string, vault common.Address) (uint64, error) {
	head, err := v.client.HeaderByNumber(v.ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("read the latest header: %w", err)
	}
	out, err := v.callVault(outDir, vault, "clock", head.Number)
	if err != nil {
		return 0, err
	}
	if got, ok := out[0].(*big.Int); !ok || got.Cmp(head.Number) != 0 {
		return 0, fmt.Errorf("clock() is %v in block %s", out[0], head.Number)
	}
	return head.Number.Uint64(), nil
}

// callVault calls a view of the vault without arguments, in the given block (nil for the
// latest), and decodes the result with the built ABI.
func (v *verifier) callVault(outDir string, vault common.Address, method string, block *big.Int) ([]any, error) {
	a, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return nil, err
	}
	data, err := a.Call(method)
	if err != nil {
		return nil, err
	}
	raw, err := v.client.CallContract(v.ctx, ethereum.CallMsg{To: &vault, Data: data}, block)
	if err != nil {
		return nil, fmt.Errorf("call %s(): %w", method, err)
	}
	out, err := a.ABI.Unpack(method, raw)
	if err != nil || len(out) != 1 {
		return nil, fmt.Errorf("decode %s(): %w", method, errors.Join(err, errors.New("want one value")))
	}
	return out, nil
}

// addresses reads the role addresses the keys directory lists; verification needs no key.
func addresses() (map[rolekeys.Role]common.Address, error) {
	raw, err := os.ReadFile(filepath.Join(vaultconf.KeysDir, rolekeys.AddressesFile))
	if err != nil {
		return nil, fmt.Errorf("read role addresses: %w", err)
	}
	var entries []rolekeys.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse role addresses: %w", err)
	}
	out := map[rolekeys.Role]common.Address{}
	for _, e := range entries {
		out[e.Role] = e.Address
	}
	for _, r := range vaultconf.Roles {
		if _, ok := out[r]; !ok {
			return nil, fmt.Errorf("no address for %s in %s", r, vaultconf.KeysDir)
		}
	}
	return out, nil
}

func (v *verifier) code(outDir, source, name string, at common.Address, immutable []byte) error {
	a, err := artifact.Load(outDir, source, name)
	if err != nil {
		return err
	}
	deployed, err := v.client.CodeAt(v.ctx, at, nil)
	if err != nil {
		return fmt.Errorf("read code: %w", err)
	}
	values, err := bytecode.Compare(a.Runtime, deployed, a.Immutables)
	if err != nil {
		return err
	}
	if len(values) > 0 && immutable == nil {
		return fmt.Errorf("%d immutables, none expected", len(values))
	}
	for i, value := range values {
		if !bytes.Equal(value, immutable) {
			return fmt.Errorf("immutable %d is %x, want %x", i, value, immutable)
		}
	}
	return nil
}

func (v *verifier) initializeRefused(outDir string, implementation common.Address, roles map[rolekeys.Role]common.Address) error {
	a, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	data, err := a.Call("initialize", roles[vaultconf.Deployer], big.NewInt(1), roles[vaultconf.Operator], roles[vaultconf.Guardian], uint64(vaultconf.WithdrawalDelay))
	if err != nil {
		return err
	}
	_, err = v.client.CallContract(v.ctx, ethereum.CallMsg{From: roles[vaultconf.Deployer], To: &implementation, Data: data}, nil)
	if err == nil {
		return errors.New("initialize would succeed on the implementation")
	}
	var de rpc.DataError
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok && strings.HasPrefix(strings.TrimPrefix(s, "0x"), invalidInitialization) {
			return nil
		}
	}
	return fmt.Errorf("refused, but not with InvalidInitialization: %v", err)
}

func (v *verifier) address(name string, contract common.Address, signature string, want common.Address) {
	w, err := chaincall.Word(v.ctx, v.client, contract, signature)
	if err == nil {
		var got common.Address
		if got, err = chaincall.AddressOf(w); err == nil {
			err = equal(got, want)
		}
	}
	v.report(name, err, want.Hex())
}

func (v *verifier) number(name string, contract common.Address, signature string, want uint64) {
	w, err := chaincall.Word(v.ctx, v.client, contract, signature)
	if err == nil {
		got := chaincall.UintOf(w)
		if got.Cmp(new(big.Int).SetUint64(want)) != 0 {
			err = fmt.Errorf("%s, want %d", got, want)
		}
	}
	v.report(name, err, fmt.Sprint(want))
}

func (v *verifier) role(name string, contract common.Address, role common.Hash, account common.Address, want bool) {
	words, err := chaincall.Words(v.ctx, v.client, contract, "hasRole(bytes32,address)", role, chaincall.AddressWord(account))
	if err == nil {
		got := len(words) == 1 && words[0] != (common.Hash{})
		if got != want {
			err = fmt.Errorf("hasRole is %t for %s", got, account)
		}
	}
	v.report(name, err, account.Hex())
}

func (v *verifier) tokenConfig(vault common.Address) {
	want := vaultconf.NativeConfig()
	words, err := chaincall.Words(v.ctx, v.client, vault, "tokenConfig(address)", chaincall.AddressWord(vaultconf.Native))
	if err == nil {
		if len(words) != 4 {
			err = fmt.Errorf("%d words, want 4", len(words))
		} else {
			got := []*big.Int{chaincall.UintOf(words[0]), chaincall.UintOf(words[1]), chaincall.UintOf(words[2]), chaincall.UintOf(words[3])}
			exp := []*big.Int{big.NewInt(1), want.Capacity, new(big.Int).SetUint64(want.RefillPeriod), want.LargeThreshold}
			for i := range got {
				if got[i].Cmp(exp[i]) != 0 {
					err = fmt.Errorf("field %d is %s, want %s", i, got[i], exp[i])
					break
				}
			}
		}
	}
	v.report("ETH configuration (allowed, capacity, refill, threshold)", err,
		fmt.Sprintf("true, %s, %d, %s", want.Capacity, want.RefillPeriod, want.LargeThreshold))
}

func equal[T comparable](got, want T) error {
	if got != want {
		return fmt.Errorf("%v, want %v", got, want)
	}
	return nil
}
