package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/WebOleg/settled/internal/kmsdev"
)

// metrics counts the service's calls, in the Prometheus text format: by operation,
// principal and outcome, the way a cloud KMS's calls are counted from its audit log.
type metrics struct {
	mu    sync.Mutex
	calls map[[3]string]uint64
}

func newMetrics(principals []string) *metrics {
	m := &metrics{calls: map[[3]string]uint64{}}
	// A denied call is reported from the start, so the first one is an increase.
	for _, op := range kmsdev.Operations {
		for _, p := range append(principals, "") {
			m.calls[[3]string{op, p, kmsdev.OutcomeDenied}] = 0
		}
	}
	return m
}

func (m *metrics) observe(operation, principal, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[[3]string{operation, principal, outcome}]++
}

func (m *metrics) write(out io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([][3]string, 0, len(m.calls))
	denied := uint64(0)
	for k, n := range m.calls {
		keys = append(keys, k)
		if k[2] == kmsdev.OutcomeDenied {
			denied += n
		}
	}
	sort.Slice(keys, func(i, j int) bool { return strings.Join(keys[i][:], " ") < strings.Join(keys[j][:], " ") })
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP kms_requests_total Calls to the KMS, by operation, principal (empty when none was recognised) and outcome.\n# TYPE kms_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "kms_requests_total{operation=%q,principal=%q,outcome=%q} %d\n", k[0], k[1], k[2], m.calls[k])
	}
	fmt.Fprintf(&b, "# HELP kms_denied_total Calls denied: a caller not who it says, or asking what its policy does not allow.\n# TYPE kms_denied_total counter\nkms_denied_total %d\n", denied)
	_, err := io.WriteString(out, b.String())
	return err
}
