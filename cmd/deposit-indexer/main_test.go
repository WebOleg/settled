package main

import (
	"strings"
	"testing"
	"time"

	"github.com/WebOleg/settled/internal/deposits"
)

func TestMetrics_reportWhatTheStepsDid(t *testing.T) {
	m := &metrics{}
	fork := uint64(40)
	m.stepped(deposits.Report{Head: 50, Finalized: 30, Tip: 48, Seen: 3, Posted: 1})
	m.stepped(deposits.Report{Head: 51, Finalized: 31, Tip: 51, Voided: 1, Fork: &fork, Settled: 2, Cancelled: 1, Suspense: 1, Backfilled: 40, Backlog: 7})
	m.check(checked{pendingDeposits: 2, suspenseWei: "1000", suspenseOpen: 3, suspenseOldest: 90 * time.Minute})
	m.failed()
	var b strings.Builder
	if err := m.write(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"deposit_indexer_deployed 1\n",
		"deposit_indexer_head_block 51\n",
		"deposit_indexer_lag_blocks 0\n",
		"deposit_indexer_deposits_seen_total 3\n",
		"deposit_indexer_deposits_posted_total 1\n",
		"deposit_indexer_deposits_voided_total 1\n",
		"deposit_indexer_reorgs_total 1\n",
		"deposit_indexer_steps_total 2\n",
		"deposit_indexer_step_errors_total 1\n",
		"deposit_indexer_finality_violation 0\n",
		"deposit_indexer_pending_deposits 2\n",
		"ledger_check_problems 0\n",
		"deposit_indexer_withdrawals_settled_total 2\n",
		"deposit_indexer_withdrawals_cancelled_total 1\n",
		"deposit_indexer_payments_to_suspense_total 1\n",
		"ledger_suspense_wei 1000\n",
		"ledger_suspense_open 3\n",
		"ledger_suspense_oldest_seconds 5400\n",
		"deposit_indexer_backfilled_blocks_total 40\n",
		"deposit_indexer_backfill_backlog_blocks 7\n",
		"# TYPE deposit_indexer_reorgs_total counter\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, b.String())
		}
	}
	m.violate()
	if !m.violation() {
		t.Fatal("a finality violation is not kept")
	}
}
