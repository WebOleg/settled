package main

import (
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/WebOleg/settled/internal/withdrawals"
)

// metrics is what the payer has done, in the Prometheus text format.
type metrics struct {
	mu                                                 sync.Mutex
	deployed                                           bool
	signed, resent, bumped, capped, rejected, reopened uint64
	refused                                            uint64
	onChain, closed                                    uint64
	steps, errors                                      uint64
	lastStep, lastCheck                                time.Time
	waits                                              waiting
}

// waiting is what the last reading found waiting.
type waiting struct {
	// open withdrawals are requested or on chain, not yet final; the oldest requested,
	// with nothing on chain yet, has waited oldestRequested seconds.
	open            int64
	oldestRequested float64
	openTxs         int64
	// balance is what the payer's key holds for gas, in wei.
	balance *big.Int
}

func (m *metrics) stepped(r withdrawals.Report, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = true
	m.signed += uint64(r.Signed)
	m.resent += uint64(r.Resent)
	m.bumped += uint64(r.Bumped)
	m.capped += uint64(r.Capped)
	m.refused += uint64(r.Refused)
	m.rejected += uint64(r.Rejected)
	m.reopened += uint64(r.Reopened)
	m.onChain += uint64(r.OnChain)
	m.closed += uint64(r.Closed)
	if !ok {
		m.errors++
		return
	}
	m.steps++
	m.lastStep = time.Now()
}

func (m *metrics) failed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors++
}

func (m *metrics) noVault() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = false
	m.lastStep = time.Now()
}

func (m *metrics) check(w waiting) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waits = w
	m.lastCheck = time.Now()
}

func (m *metrics) write(out io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	metric := func(kind, name, help string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}
	flag := 0
	if m.deployed {
		flag = 1
	}
	unix := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	balance := "0"
	if m.waits.balance != nil {
		balance = m.waits.balance.String()
	}
	metric("gauge", "withdrawal_payer_deployed", "1 when a deployment record names a vault to pay from.", flag)
	metric("counter", "withdrawal_payer_signed_total", "Transactions signed for a withdrawal the vault had nothing for, or a queued one released.", m.signed)
	metric("counter", "withdrawal_payer_resent_total", "Recorded transactions sent again, the same bytes.", m.resent)
	metric("counter", "withdrawal_payer_replaced_total", "Transactions replaced under the same nonce with higher fees.", m.bumped)
	metric("counter", "withdrawal_payer_capped_total", "Times a transaction could not be replaced: a tenth more would pass the fee cap.", m.capped)
	metric("counter", "withdrawal_payer_refused_total", "Transactions the signer would not sign under its policy; nothing was recorded for them.", m.refused)
	metric("counter", "withdrawal_payer_rejected_total", "Withdrawals the vault refuses for good; their holds released.", m.rejected)
	metric("counter", "withdrawal_payer_reopened_total", "Withdrawals a reorganisation took off the chain, paid again.", m.reopened)
	metric("counter", "withdrawal_payer_on_chain_total", "Withdrawals the vault was seen to have paid or queued.", m.onChain)
	metric("counter", "withdrawal_payer_closed_total", "Transactions closed: a version final, or its nonce used by a final block.", m.closed)
	metric("counter", "withdrawal_payer_steps_total", "Steps that succeeded.", m.steps)
	metric("counter", "withdrawal_payer_step_errors_total", "Steps that failed; what a failed step recorded is sent by the next.", m.errors)
	metric("gauge", "withdrawal_payer_last_step_timestamp_seconds", "When a step last succeeded.", unix(m.lastStep))
	metric("gauge", "withdrawal_payer_open_withdrawals", "Withdrawals requested or on chain, not yet final.", m.waits.open)
	metric("gauge", "withdrawal_payer_oldest_requested_seconds", "How long the oldest withdrawal with nothing on chain yet has waited; 0 when none does.", int64(m.waits.oldestRequested))
	metric("gauge", "withdrawal_payer_open_transactions", "Transactions recorded and not yet closed.", m.waits.openTxs)
	metric("gauge", "withdrawal_payer_balance_wei", "What the payer's key holds to pay gas with, in wei.", balance)
	metric("gauge", "withdrawal_payer_check_timestamp_seconds", "When what waits was last read.", unix(m.lastCheck))
	_, err := io.WriteString(out, b.String())
	return err
}
