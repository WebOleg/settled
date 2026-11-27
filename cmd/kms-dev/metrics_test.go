package main

import (
	"strings"
	"testing"
)

func TestMetrics_aDeniedCallIsAnIncreaseFromTheStart(t *testing.T) {
	m := newMetrics([]string{"admin", "withdrawal-signer"})
	var b strings.Builder
	if err := m.write(&b); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`kms_requests_total{operation="Sign",principal="admin",outcome="denied"} 0`, "kms_denied_total 0"} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("before any call, no %q", line)
		}
	}
	m.observe("Sign", "withdrawal-signer", "ok")
	m.observe("Sign", "admin", "denied")
	m.observe("Sign", "", "denied")
	b.Reset()
	if err := m.write(&b); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`kms_requests_total{operation="Sign",principal="withdrawal-signer",outcome="ok"} 1`,
		`kms_requests_total{operation="Sign",principal="admin",outcome="denied"} 1`, `kms_requests_total{operation="Sign",principal="",outcome="denied"} 1`, "kms_denied_total 2"} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("no %q in\n%s", line, b.String())
		}
	}
}
