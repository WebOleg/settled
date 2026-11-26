package rolekeys

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "keys")
}

func TestEnsureCreatesOwnerOnlyKeys(t *testing.T) {
	dir := newDir(t)
	entries, err := Ensure(dir, All)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(All) {
		t.Fatalf("%d entries, want %d", len(entries), len(All))
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o, want 700", info.Mode().Perm())
	}
	for _, e := range entries {
		info, err := os.Stat(keyPath(dir, e.Role))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s key mode %o, want 600", e.Role, info.Mode().Perm())
		}
		key, err := Load(dir, e.Role)
		if err != nil {
			t.Fatal(err)
		}
		if crypto.PubkeyToAddress(key.PublicKey) != e.Address {
			t.Errorf("%s: address does not match its key", e.Role)
		}
	}
}

func TestEnsureKeepsExistingKeys(t *testing.T) {
	dir := newDir(t)
	first, err := Ensure(dir, All)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(dir, All)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("%s changed from %s to %s", first[i].Role, first[i].Address, second[i].Address)
		}
	}
}

func TestEnsureWritesAddressesFile(t *testing.T) {
	dir := newDir(t)
	entries, err := Ensure(dir, All)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, AddressesFile))
	if err != nil {
		t.Fatal(err)
	}
	var got []Entry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(entries) {
		t.Fatalf("addresses.json has %d entries, want %d", len(got), len(entries))
	}
	for i := range got {
		if got[i] != entries[i] {
			t.Errorf("entry %d: %+v, want %+v", i, got[i], entries[i])
		}
	}
}

func TestLoadRefusesKeyReadableByOthers(t *testing.T) {
	dir := newDir(t)
	if _, err := Ensure(dir, []Role{Batcher}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath(dir, Batcher), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, Batcher); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("got %v, want ErrInsecurePermissions", err)
	}
	if _, err := Ensure(dir, []Role{Batcher}); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Ensure: got %v, want ErrInsecurePermissions", err)
	}
}

func TestEnsureRefusesOpenDirectory(t *testing.T) {
	dir := newDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, All); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("got %v, want ErrInsecurePermissions", err)
	}
}

func TestLoadMissingKey(t *testing.T) {
	dir := newDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, Proposer); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist", err)
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	dir := newDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath(dir, Proposer), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, Proposer); err == nil {
		t.Fatal("garbage key file loaded without error")
	}
}
