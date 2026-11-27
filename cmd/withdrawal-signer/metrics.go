package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// rules are the policy's rules, reported from the start so a refusal under any of them
// is an increase, not a new series.
var rules = []string{"type", "chain", "destination", "value", "access list", "gas", "fee cap", "method", "ledger", "velocity", "client"}

// metrics is what the signer has decided, in the Prometheus text format.
type metrics struct {
	mu       sync.Mutex
	signed   uint64
	refused  map[string]uint64
	last     time.Time
	lastRule string
}

func newMetrics() *metrics {
	m := &metrics{refused: map[string]uint64{}}
	for _, r := range rules {
		m.refused[r] = 0
	}
	return m
}

func (m *metrics) decided(decision, rule string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if decision == "signed" {
		m.signed++
	} else {
		m.refused[rule]++
		m.lastRule = rule
	}
	m.last = time.Now()
}

func (m *metrics) write(out io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	total := uint64(0)
	names := make([]string, 0, len(m.refused))
	for r, n := range m.refused {
		names = append(names, r)
		total += n
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "# HELP withdrawal_signer_signed_total Transactions signed: allowed by the policy and the ledger.\n# TYPE withdrawal_signer_signed_total counter\nwithdrawal_signer_signed_total %d\n", m.signed)
	fmt.Fprintf(&b, "# HELP withdrawal_signer_refusals_total Transactions refused, under any rule.\n# TYPE withdrawal_signer_refusals_total counter\nwithdrawal_signer_refusals_total %d\n", total)
	fmt.Fprintf(&b, "# HELP withdrawal_signer_refused_total Transactions refused, by the rule they broke.\n# TYPE withdrawal_signer_refused_total counter\n")
	for _, r := range names {
		fmt.Fprintf(&b, "withdrawal_signer_refused_total{rule=%q} %d\n", r, m.refused[r])
	}
	last := int64(0)
	if !m.last.IsZero() {
		last = m.last.Unix()
	}
	fmt.Fprintf(&b, "# HELP withdrawal_signer_last_decision_timestamp_seconds When the signer last decided.\n# TYPE withdrawal_signer_last_decision_timestamp_seconds gauge\nwithdrawal_signer_last_decision_timestamp_seconds %d\n", last)
	_, err := io.WriteString(out, b.String())
	return err
}
