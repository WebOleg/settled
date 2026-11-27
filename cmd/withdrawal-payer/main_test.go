package main

import (
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/withdrawals"
)

func TestMetrics_reportWhatTheStepsDid(t *testing.T) {
	m := &metrics{}
	m.stepped(withdrawals.Report{Signed: 2, OnChain: 1}, true)
	m.stepped(withdrawals.Report{Resent: 1, Bumped: 1, Capped: 1, Closed: 3}, true)
	m.stepped(withdrawals.Report{}, false)
	m.check(waiting{open: 4, oldestRequested: 61.5, openTxs: 2, balance: big.NewInt(7)})
	var b strings.Builder
	if err := m.write(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"withdrawal_payer_deployed 1\n",
		"withdrawal_payer_signed_total 2\n",
		"withdrawal_payer_resent_total 1\n",
		"withdrawal_payer_replaced_total 1\n",
		"withdrawal_payer_capped_total 1\n",
		"withdrawal_payer_on_chain_total 1\n",
		"withdrawal_payer_closed_total 3\n",
		"withdrawal_payer_steps_total 2\n",
		"withdrawal_payer_step_errors_total 1\n",
		"withdrawal_payer_open_withdrawals 4\n",
		"withdrawal_payer_oldest_requested_seconds 61\n",
		"withdrawal_payer_open_transactions 2\n",
		"withdrawal_payer_balance_wei 7\n",
		"# TYPE withdrawal_payer_signed_total counter\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

func TestReadKey_neverRepeatsTheFile(t *testing.T) {
	dir := t.TempDir()
	key, _ := crypto.GenerateKey()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("0x"+hex.EncodeToString(crypto.FromECDSA(key))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readKey(good)
	if err != nil || crypto.PubkeyToAddress(got.PublicKey) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("readKey: %v", err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not-a-key-but-a-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(bad); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("readKey of a bad file: %v, want an error that does not repeat it", err)
	}
}
