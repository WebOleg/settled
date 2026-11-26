// Package rolekeys creates and loads one private key per operator role of the devnet L2.
//
// Keys live in one directory, one file per role, readable only by the owner. A key file
// is never overwritten, and a key that others can read is refused. Only addresses are
// ever printed or written in clear, to addresses.json.
package rolekeys

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Role is a named operator account.
type Role string

// The operator roles of the devnet L2, in the order they are listed everywhere.
const (
	Deployer          Role = "deployer"
	SuperchainAdmin   Role = "superchain-admin"
	Guardian          Role = "guardian"
	ChainAdmin        Role = "chain-admin"
	SystemConfigOwner Role = "system-config-owner"
	Sequencer         Role = "sequencer"
	Batcher           Role = "batcher"
	Proposer          Role = "proposer"
	Challenger        Role = "challenger"
	FeeRecipient      Role = "fee-recipient"
)

// All lists every role.
var All = []Role{
	Deployer, SuperchainAdmin, Guardian, ChainAdmin, SystemConfigOwner,
	Sequencer, Batcher, Proposer, Challenger, FeeRecipient,
}

// AddressesFile is the public list of role addresses inside the key directory.
const AddressesFile = "addresses.json"

// ErrInsecurePermissions is returned for a key file or key directory that others can
// read or enter.
var ErrInsecurePermissions = errors.New("rolekeys: permissions allow access by others")

// Entry is one line of addresses.json.
type Entry struct {
	Role    Role           `json:"role"`
	Address common.Address `json:"address"`
}

// Ensure makes sure dir holds a key for every role, creating the missing ones, and
// rewrites addresses.json. Existing keys are kept. It returns the addresses in the
// order of roles.
func Ensure(dir string, roles []Role) ([]Entry, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(roles))
	seen := make(map[common.Address]Role, len(roles))
	for _, role := range roles {
		key, err := Load(dir, role)
		if errors.Is(err, fs.ErrNotExist) {
			key, err = create(dir, role)
		}
		if err != nil {
			return nil, err
		}
		addr := crypto.PubkeyToAddress(key.PublicKey)
		if other, dup := seen[addr]; dup {
			return nil, fmt.Errorf("rolekeys: %s and %s share address %s", other, role, addr)
		}
		seen[addr] = role
		entries = append(entries, Entry{Role: role, Address: addr})
	}
	if err := writeAddresses(dir, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// Load reads the key of role from dir. It fails with an error wrapping fs.ErrNotExist
// when the key does not exist, and with ErrInsecurePermissions when others can read it.
func Load(dir string, role Role) (*ecdsa.PrivateKey, error) {
	path := keyPath(dir, role)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("rolekeys: %s key: %w", role, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s has mode %o, want 600", ErrInsecurePermissions, path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rolekeys: read %s key: %w", role, err)
	}
	key, err := crypto.HexToECDSA(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("rolekeys: parse %s key: %w", role, err)
	}
	return key, nil
}

func keyPath(dir string, role Role) string {
	return filepath.Join(dir, string(role)+".key")
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("rolekeys: create %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("rolekeys: stat %s: %w", dir, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o, want 700", ErrInsecurePermissions, dir, info.Mode().Perm())
	}
	return nil
}

// create writes a new key with O_EXCL, so an existing file is never replaced, not even
// by a second process racing this one.
func create(dir string, role Role) (*ecdsa.PrivateKey, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("rolekeys: generate %s key: %w", role, err)
	}
	f, err := os.OpenFile(keyPath(dir, role), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rolekeys: create %s key file: %w", role, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(crypto.FromECDSA(key)) + "\n"); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("rolekeys: write %s key: %w", role, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("rolekeys: close %s key: %w", role, err)
	}
	return key, nil
}

// writeAddresses replaces addresses.json through a temporary file and a rename, so a
// reader never sees a half-written list.
func writeAddresses(dir string, entries []Entry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("rolekeys: encode addresses: %w", err)
	}
	tmp, err := os.CreateTemp(dir, AddressesFile+".*")
	if err != nil {
		return fmt.Errorf("rolekeys: temp file for addresses: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rolekeys: write addresses: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rolekeys: close addresses: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("rolekeys: chmod addresses: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, AddressesFile)); err != nil {
		return fmt.Errorf("rolekeys: replace addresses: %w", err)
	}
	return nil
}
