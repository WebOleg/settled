package artifact

import (
	"bytes"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `{
  "abi": [
    {"type": "constructor", "inputs": [{"name": "x", "type": "uint256"}], "stateMutability": "nonpayable"},
    {"type": "function", "name": "set", "inputs": [{"name": "x", "type": "uint256"}], "outputs": [], "stateMutability": "nonpayable"}
  ],
  "bytecode": {"object": "0x6001", "linkReferences": {}},
  "deployedBytecode": {"object": "0x60aa60bb", "immutableReferences": {"7": [{"start": 3, "length": 1}], "3": [{"start": 1, "length": 1}]}}
}`

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "A.sol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "A.sol", "A.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoad(t *testing.T) {
	a, err := Load(write(t, fixture), "A.sol", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Creation, []byte{0x60, 0x01}) || !bytes.Equal(a.Runtime, []byte{0x60, 0xaa, 0x60, 0xbb}) {
		t.Fatalf("code: %x %x", a.Creation, a.Runtime)
	}
	if len(a.Immutables) != 2 || a.Immutables[0].Start != 1 || a.Immutables[1].Start != 3 {
		t.Fatalf("immutables not sorted by start: %+v", a.Immutables)
	}
}

func TestDeployDataAppendsArguments(t *testing.T) {
	a, err := Load(write(t, fixture), "A.sol", "A")
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.DeployData(bigOne())
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2+32 || data[len(data)-1] != 1 || !bytes.Equal(data[:2], a.Creation) {
		t.Fatalf("deploy data %x", data)
	}
	// The creation code itself is not changed by appending.
	if len(a.Creation) != 2 {
		t.Fatal("creation code changed")
	}
}

func TestLoadRefusesLinkedLibrariesAndMissingFiles(t *testing.T) {
	linked := strings.Replace(fixture, `"linkReferences": {}`, `"linkReferences": {"src/L.sol": {"L": [{"start": 1, "length": 20}]}}`, 1)
	if _, err := Load(write(t, linked), "A.sol", "A"); err == nil || !strings.Contains(err.Error(), "linked libraries") {
		t.Fatalf("linked: err = %v", err)
	}
	if _, err := Load(t.TempDir(), "A.sol", "A"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func bigOne() any {
	return new(big.Int).SetUint64(1)
}
