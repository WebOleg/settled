package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/WebOleg/settled/internal/deposits"
)

// metrics is what the indexer has done, in the Prometheus text format.
type metrics struct {
	mu                    sync.Mutex
	deployed              bool
	head, finalized, tip  uint64
	seen, posted, voided  uint64
	settled, cancelled    uint64
	suspense              uint64
	backfilled, backlog   uint64
	reorgs, steps, errors uint64
	lastStep              time.Time
	finalityViolation     bool
	lastCheck             time.Time
	checks                checked
}

// checked is what the last check of the books found.
type checked struct {
	problems        int
	pendingDeposits int64
	suspenseWei     string
	// suspenseOpen payments in suspense are not yet explained; the oldest has waited
	// suspenseOldest.
	suspenseOpen   int64
	suspenseOldest time.Duration
}

func (m *metrics) stepped(r deposits.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = true
	m.head, m.finalized, m.tip = r.Head, r.Finalized, r.Tip
	m.seen += uint64(r.Seen)
	m.posted += uint64(r.Posted)
	m.voided += uint64(r.Voided)
	m.settled += uint64(r.Settled)
	m.cancelled += uint64(r.Cancelled)
	m.suspense += uint64(r.Suspense)
	m.backfilled += r.Backfilled
	m.backlog = r.Backlog
	if r.Fork != nil {
		m.reorgs++
	}
	m.steps++
	m.lastStep = time.Now()
}

func (m *metrics) noVault() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deployed = false
	m.steps++
	m.lastStep = time.Now()
}

func (m *metrics) failed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors++
}

func (m *metrics) violate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finalityViolation = true
	m.errors++
}

func (m *metrics) violation() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.finalityViolation
}

func (m *metrics) check(c checked) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checks = c
	m.lastCheck = time.Now()
}

func (m *metrics) write(out io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	metric := func(kind, name, help string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}
	flag := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	unix := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.Unix()
	}
	lag := uint64(0)
	if m.head > m.tip {
		lag = m.head - m.tip
	}
	metric("gauge", "deposit_indexer_deployed", "1 when a deployment record names a vault to follow.", flag(m.deployed))
	metric("gauge", "deposit_indexer_head_block", "The node's latest L2 block at the last step.", m.head)
	metric("gauge", "deposit_indexer_finalized_block", "The node's finalized L2 block at the last step.", m.finalized)
	metric("gauge", "deposit_indexer_tip_block", "The highest L2 block the indexer has read.", m.tip)
	metric("gauge", "deposit_indexer_lag_blocks", "Blocks between the node's head and the indexer's tip.", lag)
	metric("counter", "deposit_indexer_deposits_seen_total", "Deposits recorded, held until final.", m.seen)
	metric("counter", "deposit_indexer_deposits_posted_total", "Deposits credited, once final.", m.posted)
	metric("counter", "deposit_indexer_deposits_voided_total", "Held deposits voided because their block left the chain.", m.voided)
	metric("counter", "deposit_indexer_withdrawals_settled_total", "Withdrawals of the ledger paid and final: their holds posted.", m.settled)
	metric("counter", "deposit_indexer_withdrawals_cancelled_total", "Withdrawals of the ledger cancelled and final: their holds released.", m.cancelled)
	metric("counter", "deposit_indexer_payments_to_suspense_total", "Final payments the ledger never requested, posted to suspense.", m.suspense)
	suspense := m.checks.suspenseWei
	if suspense == "" {
		suspense = "0"
	}
	metric("gauge", "ledger_suspense_wei", "What the suspense account holds: payments that left the vault without the ledger, in wei.", suspense)
	metric("gauge", "ledger_suspense_open", "Payments in suspense not yet explained by an approved resolution.", m.checks.suspenseOpen)
	metric("gauge", "ledger_suspense_oldest_seconds", "How long the oldest payment in suspense not yet explained has waited; 0 when none does.",
		int64(m.checks.suspenseOldest.Seconds()))
	metric("counter", "deposit_indexer_backfilled_blocks_total", "Final blocks read again for a stream added after they were read.", m.backfilled)
	metric("gauge", "deposit_indexer_backfill_backlog_blocks", "Blocks a stream added later still has to read again; 0 when every stream is complete.", m.backlog)
	metric("counter", "deposit_indexer_reorgs_total", "Reorganisations unwound.", m.reorgs)
	metric("counter", "deposit_indexer_steps_total", "Steps that succeeded.", m.steps)
	metric("counter", "deposit_indexer_step_errors_total", "Steps that failed; nothing of a failed step is written.", m.errors)
	metric("gauge", "deposit_indexer_last_step_timestamp_seconds", "When a step last succeeded.", unix(m.lastStep))
	metric("gauge", "deposit_indexer_finality_violation", "1 once the node's chain no longer has a block held final; the indexer has stopped.", flag(m.finalityViolation))
	metric("gauge", "deposit_indexer_pending_deposits", "Deposits held, not yet final.", m.checks.pendingDeposits)
	metric("gauge", "ledger_check_problems", "Differences between the balances and the journal at the last check; 0 when the books are right.", m.checks.problems)
	metric("gauge", "ledger_check_timestamp_seconds", "When the books were last checked against the journal.", unix(m.lastCheck))
	_, err := io.WriteString(out, b.String())
	return err
}
