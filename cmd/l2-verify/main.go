// Command l2-verify checks a deployment of the devnet L2 against the intent, in two
// places. On the L1 it reads the contracts: each owner and operator is the role meant
// for it, the deployer holds none of them, and SystemConfig holds the expected values.
// In the exported chain config it checks the fee parameters and that genesis.json and
// rollup.json point at the contracts that were deployed. It needs no key.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/l2intent"
	"github.com/WebOleg/settled/internal/rolekeys"
)

// check is one expected value read from one contract.
type check struct {
	contract  string // key in l1-addresses.json
	signature string
	role      rolekeys.Role // expected address, if set
	asWord    bool          // the address is stored as a bytes32 word
	want      *big.Int      // expected number, if role is empty
	note      string        // why the expected number is what it is
}

var checks = []check{
	{contract: "SuperchainProxyAdminImpl", signature: "owner()", role: rolekeys.SuperchainAdmin},
	{contract: "SuperchainConfigProxy", signature: "guardian()", role: rolekeys.Guardian},
	{contract: "OpChainProxyAdminImpl", signature: "owner()", role: rolekeys.ChainAdmin},
	{contract: "DisputeGameFactoryProxy", signature: "owner()", role: rolekeys.ChainAdmin},
	{contract: "SystemConfigProxy", signature: "owner()", role: rolekeys.SystemConfigOwner},
	{contract: "SystemConfigProxy", signature: "batcherHash()", role: rolekeys.Batcher, asWord: true},
	{contract: "SystemConfigProxy", signature: "unsafeBlockSigner()", role: rolekeys.Sequencer},
	{contract: "SystemConfigProxy", signature: "gasLimit()", want: big.NewInt(l2intent.GasLimit)},
	// Holocene: SystemConfig does not set the EIP-1559 parameters at initialisation; while
	// they are unset, the L2 uses the values of its chain config (checked below).
	{contract: "SystemConfigProxy", signature: "eip1559Denominator()", want: big.NewInt(0), note: "unset; chain config applies"},
	{contract: "SystemConfigProxy", signature: "eip1559Elasticity()", want: big.NewInt(0), note: "unset; chain config applies"},
	// Jovian: 0 means the default of 400.
	{contract: "SystemConfigProxy", signature: "daFootprintGasScalar()", want: big.NewInt(0), note: "unset; Jovian uses 400"},
	{contract: "SystemConfigProxy", signature: "minBaseFee()", want: big.NewInt(0)},
	{contract: "SystemConfigProxy", signature: "operatorFeeScalar()", want: big.NewInt(0)},
}

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "L1 execution JSON-RPC endpoint")
	dir := flag.String("deployer-dir", ".devnet/l2/deployer", "directory with the files written by make l2-deploy")
	rolesFile := flag.String("roles", ".devnet/l2/keys/"+rolekeys.AddressesFile, "role addresses written by make l2-keys")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	failed, total, err := run(ctx, *rpcURL, *dir, *rolesFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "l2-verify: FAIL: %v\n", err)
		os.Exit(1)
	}
	if failed > 0 {
		fmt.Printf("l2-verify: FAIL, %d of %d checks\n", failed, total)
		os.Exit(1)
	}
	fmt.Printf("l2-verify: ok, %d checks\n", total)
}

func run(ctx context.Context, rpcURL, dir, rolesFile string) (int, int, error) {
	l1File := filepath.Join(dir, "l1-addresses.json")
	contracts := map[string]common.Address{}
	if err := readJSON(l1File, &contracts); err != nil {
		return 0, 0, err
	}
	var entries []rolekeys.Entry
	if err := readJSON(rolesFile, &entries); err != nil {
		return 0, 0, err
	}
	roles := make(map[rolekeys.Role]common.Address, len(entries))
	for _, e := range entries {
		roles[e.Role] = e.Address
	}
	deployer, ok := roles[rolekeys.Deployer]
	if !ok {
		return 0, 0, fmt.Errorf("%s has no deployer address", rolesFile)
	}

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return 0, 0, fmt.Errorf("connect to %s: %w", rpcURL, err)
	}
	defer client.Close()

	failed := 0
	deployerHolds := []string{}
	for _, c := range checks {
		name := c.contract + "." + c.signature
		addr, ok := contracts[c.contract]
		if !ok || addr == (common.Address{}) {
			fmt.Printf("FAIL  %-56s no address for %s in %s\n", name, c.contract, l1File)
			failed++
			continue
		}
		word, err := chaincall.Word(ctx, client, addr, c.signature)
		if err != nil {
			fmt.Printf("FAIL  %-56s %v\n", name, err)
			failed++
			continue
		}

		if c.role == "" {
			got := chaincall.UintOf(word)
			if got.Cmp(c.want) != 0 {
				fmt.Printf("FAIL  %-56s %s, want %s\n", name, got, c.want)
				failed++
				continue
			}
			if c.note != "" {
				fmt.Printf("ok    %-56s %s (%s)\n", name, got, c.note)
			} else {
				fmt.Printf("ok    %-56s %s\n", name, got)
			}
			continue
		}

		want := roles[c.role]
		var got common.Address
		if c.asWord {
			if word != chaincall.AddressWord(want) {
				fmt.Printf("FAIL  %-56s %s, want %s (%s)\n", name, word, chaincall.AddressWord(want), c.role)
				failed++
				continue
			}
			got = want
		} else {
			got, err = chaincall.AddressOf(word)
			if err != nil {
				fmt.Printf("FAIL  %-56s %v\n", name, err)
				failed++
				continue
			}
		}
		if got == deployer {
			deployerHolds = append(deployerHolds, name)
		}
		if got != want {
			fmt.Printf("FAIL  %-56s %s, want %s (%s)\n", name, got, want, c.role)
			failed++
			continue
		}
		fmt.Printf("ok    %-56s %s (%s)\n", name, got, c.role)
	}

	if len(deployerHolds) > 0 {
		fmt.Printf("FAIL  %-56s deployer %s holds %v\n", "deployer holds no role", deployer, deployerHolds)
		failed++
	} else {
		fmt.Printf("ok    %-56s %s holds none of the above\n", "deployer holds no role", deployer)
	}

	fileFailed, fileTotal, err := checkChainConfig(dir, contracts, roles)
	if err != nil {
		return 0, 0, err
	}
	return failed + fileFailed, len(checks) + 1 + fileTotal, nil
}

// fileCheck is one expected value at a path inside genesis.json or rollup.json.
type fileCheck struct {
	file string
	path []string
	want string
}

// checkChainConfig checks the exported chain config: the fee parameters the L2 uses
// while SystemConfig leaves them unset, and that the config points at the contracts
// that were deployed.
func checkChainConfig(dir string, contracts map[string]common.Address, roles map[rolekeys.Role]common.Address) (int, int, error) {
	docs := map[string]map[string]any{}
	for _, f := range []string{"genesis.json", "rollup.json"} {
		var doc map[string]any
		if err := readJSONNumbers(filepath.Join(dir, f), &doc); err != nil {
			return 0, 0, err
		}
		docs[f] = doc
	}
	n := func(v int) string { return fmt.Sprint(v) }
	list := []fileCheck{
		{"genesis.json", []string{"config", "chainId"}, "420420"},
		{"genesis.json", []string{"config", "optimism", "eip1559Denominator"}, n(l2intent.EIP1559Denominator)},
		{"genesis.json", []string{"config", "optimism", "eip1559DenominatorCanyon"}, n(l2intent.EIP1559DenominatorCanyon)},
		{"genesis.json", []string{"config", "optimism", "eip1559Elasticity"}, n(l2intent.EIP1559Elasticity)},
		{"rollup.json", []string{"l2_chain_id"}, "420420"},
		{"rollup.json", []string{"l1_chain_id"}, "3151908"},
		{"rollup.json", []string{"chain_op_config", "eip1559Denominator"}, n(l2intent.EIP1559Denominator)},
		{"rollup.json", []string{"chain_op_config", "eip1559DenominatorCanyon"}, n(l2intent.EIP1559DenominatorCanyon)},
		{"rollup.json", []string{"chain_op_config", "eip1559Elasticity"}, n(l2intent.EIP1559Elasticity)},
		{"rollup.json", []string{"l1_system_config_address"}, contracts["SystemConfigProxy"].Hex()},
		{"rollup.json", []string{"deposit_contract_address"}, contracts["OptimismPortalProxy"].Hex()},
		{"rollup.json", []string{"genesis", "system_config", "batcherAddr"}, roles[rolekeys.Batcher].Hex()},
	}
	failed := 0
	for _, c := range list {
		name := c.file + " " + strings.Join(c.path, ".")
		got, ok := lookup(docs[c.file], c.path)
		switch {
		case !ok:
			fmt.Printf("FAIL  %-56s not present\n", name)
			failed++
		case !strings.EqualFold(got, c.want):
			fmt.Printf("FAIL  %-56s %s, want %s\n", name, got, c.want)
			failed++
		default:
			fmt.Printf("ok    %-56s %s\n", name, got)
		}
	}
	return failed, len(list), nil
}

// lookup follows path through nested JSON objects and returns the value as text.
func lookup(doc map[string]any, path []string) (string, bool) {
	var cur any = doc
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = obj[key]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	default:
		return "", false
	}
}

// readJSONNumbers parses JSON keeping numbers as text, so large values stay exact.
func readJSONNumbers(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
