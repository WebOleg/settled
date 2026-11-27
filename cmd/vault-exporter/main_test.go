package main

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/vaultwatch"
)

// The watcher carries the vault's event topics and selectors as constants; they must be
// the keccak256 of the signatures in IVault.
func TestTopicsAndSelectorsAreTheVaultsOwn(t *testing.T) {
	for sig, want := range map[string]string{
		"WithdrawalQueued(bytes32,address,address,uint256,uint64)": vaultwatch.TopicQueued,
		"WithdrawalPaid(bytes32,address,address,uint256)":          vaultwatch.TopicPaid,
		"WithdrawalCancelled(bytes32,address)":                     vaultwatch.TopicCancelled,
	} {
		if got := crypto.Keccak256Hash([]byte(sig)).Hex(); got != want {
			t.Errorf("%s: topic %s, the constant says %s", sig, got, want)
		}
	}
	for sig, want := range map[string]string{"paused()": vaultwatch.SelectorPaused, "accounted(address)": vaultwatch.SelectorAccounted} {
		if got := "0x" + strings.TrimPrefix(crypto.Keccak256Hash([]byte(sig)).Hex(), "0x")[:8]; got != want {
			t.Errorf("%s: selector %s, the constant says %s", sig, got, want)
		}
	}
}
