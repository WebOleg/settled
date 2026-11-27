package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestMetrics_everyRuleFromTheStart(t *testing.T) {
	m := newMetrics()
	m.decided("signed", "allowed")
	m.decided("refused", "ledger")
	m.decided("refused", "ledger")
	var b strings.Builder
	if err := m.write(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"withdrawal_signer_signed_total 1\n",
		"withdrawal_signer_refusals_total 2\n",
		`withdrawal_signer_refused_total{rule="ledger"} 2` + "\n",
		`withdrawal_signer_refused_total{rule="velocity"} 0` + "\n",
		`withdrawal_signer_refused_total{rule="access list"} 0` + "\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, b.String())
		}
	}
}

func TestWaitRecord_signsOnlyAsThePayerTheRecordNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	as := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	write := func(payer string) {
		t.Helper()
		body := `{"chainId": 901, "vault": "0x00000000000000000000000000000000000000b2", "block": 7` + payer + `}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wait := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := waitRecord(ctx, path, as)
		return err
	}
	if err := wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no record: %v, want to wait", err)
	}
	write("")
	if err := wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no payer in the record: %v, want to wait", err)
	}
	write(`, "payer": {"address": "0x00000000000000000000000000000000000000c3"}`)
	if err := wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another payer: %v, want to wait", err)
	}
	write(`, "payer": {"address": "not an address"}`)
	if err := wait(); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a payer that is not an address: %v, want an error", err)
	}
	write(`, "payer": {"address": "` + strings.ToLower(as.Hex()) + `"}`)
	rec, err := waitRecord(context.Background(), path, as)
	if err != nil || rec.ChainID != 901 {
		t.Fatalf("this key's account: %+v, %v", rec, err)
	}
}
