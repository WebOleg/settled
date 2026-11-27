package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/rolekeys"
)

func TestWriteSecret_onceInAFolderOnlyItsOwnerReads(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSecret(filepath.Join(open, "payer-key.txt"), key); !errors.Is(err, rolekeys.ErrInsecurePermissions) {
		t.Fatalf("a folder others can enter: %v, want ErrInsecurePermissions", err)
	}
	dir := filepath.Join(t.TempDir(), "bridge")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "payer-key.txt")
	if written, err := writeSecret(path, key); err != nil || !written {
		t.Fatalf("first write: %t, %v", written, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the secret's mode: %v, %v; want 644 for the container's user", info.Mode().Perm(), err)
	}
	if written, err := writeSecret(path, key); err != nil || written {
		t.Fatalf("second write of the same key: %t, %v; want nothing written", written, err)
	}
	other, _ := crypto.GenerateKey()
	if written, err := writeSecret(path, other); err != nil || !written {
		t.Fatalf("a changed key: %t, %v; want it written", written, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("a temporary file is left: %v", err)
	}
}
