package main

import (
	"strings"
	"testing"
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
