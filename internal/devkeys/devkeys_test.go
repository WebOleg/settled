package devkeys

import (
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// The Hardhat and Anvil default mnemonic and its first two accounts are published
// widely, which makes them an independent check of the whole derivation.
const knownMnemonic = "test test test test test test test test test test test junk"

func TestFromMnemonicKnownAccounts(t *testing.T) {
	cases := []struct {
		index uint32
		want  string
	}{
		{0, "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"},
		{1, "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"},
	}
	for _, c := range cases {
		key, err := FromMnemonic(knownMnemonic, c.index)
		if err != nil {
			t.Fatalf("index %d: %v", c.index, err)
		}
		got := crypto.PubkeyToAddress(key.PublicKey).Hex()
		if got != c.want {
			t.Errorf("index %d: got %s, want %s", c.index, got, c.want)
		}
	}
}

func TestFromMnemonicIgnoresExtraSpaces(t *testing.T) {
	a, err := FromMnemonic(knownMnemonic, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromMnemonic("  test test test test test test test test test test test   junk \n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(a.PublicKey) != crypto.PubkeyToAddress(b.PublicKey) {
		t.Error("extra whitespace changed the derived key")
	}
}

func TestFromMnemonicRejectsEmpty(t *testing.T) {
	if _, err := FromMnemonic("   ", 0); err == nil {
		t.Error("empty mnemonic: want an error, got none")
	}
}
