// Package artifact reads the build output forge writes for a contract: its ABI, its
// creation code, its runtime code, where the runtime code holds immutables, and its
// storage layout. Commands
// that deploy or verify contracts take the code from here, so what goes on chain and
// what is checked against it is what the repository builds.
package artifact

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/WebOleg/settled/internal/bytecode"
)

// Artifact is one contract from forge's out directory.
type Artifact struct {
	Name       string
	ABI        abi.ABI
	Creation   []byte
	Runtime    []byte
	Immutables []bytecode.Range
	// StorageLayout is solc's storageLayout output, raw; empty when the build did not
	// ask for it (foundry.toml extra_output).
	StorageLayout json.RawMessage
}

type file struct {
	ABI      json.RawMessage `json:"abi"`
	Bytecode struct {
		Object         string          `json:"object"`
		LinkReferences json.RawMessage `json:"linkReferences"`
	} `json:"bytecode"`
	DeployedBytecode struct {
		Object              string                      `json:"object"`
		ImmutableReferences map[string][]bytecode.Range `json:"immutableReferences"`
	} `json:"deployedBytecode"`
	StorageLayout json.RawMessage `json:"storageLayout"`
}

// Load reads out/<source>/<name>.json, for example Load("contracts/out", "Vault.sol",
// "Vault"). A contract that needs linked libraries is refused: none is used here, and
// its code would not be complete.
func Load(outDir, source, name string) (*Artifact, error) {
	path := filepath.Join(outDir, source, name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: read %s (run make check-contracts to build): %w", path, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("artifact: parse %s: %w", path, err)
	}
	if links := strings.TrimSpace(string(f.Bytecode.LinkReferences)); links != "" && links != "{}" && links != "null" {
		return nil, fmt.Errorf("artifact: %s needs linked libraries", name)
	}
	parsed, err := abi.JSON(strings.NewReader(string(f.ABI)))
	if err != nil {
		return nil, fmt.Errorf("artifact: ABI of %s: %w", name, err)
	}
	creation, err := hexutil.Decode(f.Bytecode.Object)
	if err != nil || len(creation) == 0 {
		return nil, fmt.Errorf("artifact: creation code of %s: %w", name, errors.Join(err, errors.New("empty or not hex")))
	}
	runtime, err := hexutil.Decode(f.DeployedBytecode.Object)
	if err != nil || len(runtime) == 0 {
		return nil, fmt.Errorf("artifact: runtime code of %s: %w", name, errors.Join(err, errors.New("empty or not hex")))
	}
	var immutables []bytecode.Range
	for _, ranges := range f.DeployedBytecode.ImmutableReferences {
		immutables = append(immutables, ranges...)
	}
	sort.Slice(immutables, func(i, j int) bool { return immutables[i].Start < immutables[j].Start })
	return &Artifact{Name: name, ABI: parsed, Creation: creation, Runtime: runtime, Immutables: immutables, StorageLayout: f.StorageLayout}, nil
}

// DeployData is the creation code followed by the ABI-encoded constructor arguments.
func (a *Artifact) DeployData(args ...any) ([]byte, error) {
	encoded, err := a.ABI.Pack("", args...)
	if err != nil {
		return nil, fmt.Errorf("artifact: constructor arguments of %s: %w", a.Name, err)
	}
	return append(append([]byte{}, a.Creation...), encoded...), nil
}

// Call is the calldata of method with args.
func (a *Artifact) Call(method string, args ...any) ([]byte, error) {
	data, err := a.ABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("artifact: %s.%s: %w", a.Name, method, err)
	}
	return data, nil
}
