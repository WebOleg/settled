package vaultwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const vault = "0x00000000000000000000000000000000000000aa"

// chain is a fake L2 node: blocks with hashes, the vault's logs, and its state.
type chain struct {
	mu        sync.Mutex
	head      uint64
	hashes    map[uint64]string
	logs      []Log
	paused    bool
	accounted *big.Int
	balance   *big.Int
	fail      bool
	calls     map[string]int
}

func newChain(head uint64) *chain {
	c := &chain{head: head, hashes: map[uint64]string{}, accounted: big.NewInt(0), balance: big.NewInt(0), calls: map[string]int{}}
	for i := uint64(0); i <= head; i++ {
		c.hashes[i] = fmt.Sprintf("0x%064x", i)
	}
	return c
}

func (c *chain) advance(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < int(n); i++ {
		c.head++
		c.hashes[c.head] = fmt.Sprintf("0x%064x", c.head)
	}
}

func hexWord(v *big.Int) string { return fmt.Sprintf("%064x", v) }

func id(n int) string { return fmt.Sprintf("0x%064x", n) }

func (c *chain) queue(block uint64, idx int, n int, amount int64, release uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, Log{Address: vault, Topics: []string{TopicQueued, id(n), id(0), id(0)},
		Data:        "0x" + hexWord(big.NewInt(amount)) + hexWord(new(big.Int).SetUint64(release)),
		BlockNumber: quantity(block), LogIndex: quantity(uint64(idx))})
}

func (c *chain) event(topic string, block uint64, idx int, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, Log{Address: vault, Topics: []string{topic, id(n), id(0), id(0)}, Data: "0x" + hexWord(big.NewInt(1)),
		BlockNumber: quantity(block), LogIndex: quantity(uint64(idx))})
}

func (c *chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[req.Method]++
	if c.fail {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	var result any
	switch req.Method {
	case "eth_getBlockByNumber":
		tag := req.Params[0].(string)
		n := c.head
		if tag != "latest" {
			v, _ := parseQuantity(tag)
			n = v
		}
		result = map[string]string{"number": quantity(n), "hash": c.hashes[n]}
	case "eth_getLogs":
		f := req.Params[0].(map[string]any)
		from, _ := parseQuantity(f["fromBlock"].(string))
		to, _ := parseQuantity(f["toBlock"].(string))
		out := []Log{}
		for _, l := range c.logs {
			b, _ := parseQuantity(l.BlockNumber)
			if b >= from && b <= to && strings.EqualFold(l.Address, f["address"].(string)) {
				out = append(out, l)
			}
		}
		result = out
	case "eth_call":
		data := req.Params[0].(map[string]any)["data"].(string)
		switch {
		case data == SelectorPaused:
			v := big.NewInt(0)
			if c.paused {
				v.SetInt64(1)
			}
			result = "0x" + hexWord(v)
		case strings.HasPrefix(data, SelectorAccounted) && strings.HasSuffix(data, Native[2:]):
			result = "0x" + hexWord(c.accounted)
		default:
			result = "0x"
		}
	case "eth_getBalance":
		result = "0x" + c.balance.Text(16)
	default:
		http.Error(w, "unknown method "+req.Method, http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(result)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(raw)})
}

func setup(t *testing.T, c *chain, record bool) (*Watcher, string) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "deployment.json")
	if record {
		writeRecord(t, path, vault, 10)
	}
	return &Watcher{Client: &Client{URL: srv.URL}, RecordPath: path}, path
}

func writeRecord(t *testing.T, path, v string, block uint64) {
	t.Helper()
	raw := fmt.Sprintf(`{"chainId": 420420, "vault": %q, "block": %d}`, v, block)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// metrics polls and returns the metrics as name -> value.
func metrics(t *testing.T, w *Watcher) map[string]string {
	t.Helper()
	if err := w.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	return render(t, w)
}

func render(t *testing.T, w *Watcher) map[string]string {
	t.Helper()
	var b strings.Builder
	if err := w.WriteMetrics(&b); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(b.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			t.Fatalf("malformed line %q", line)
		}
		out[f[0]] = f[1]
	}
	return out
}

func expect(t *testing.T, m map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
}

func TestNoRecordIsNothingToWatch(t *testing.T) {
	w, _ := setup(t, newChain(20), false)
	expect(t, metrics(t, w), map[string]string{"vault_deployed": "0", "vault_solvent": "1", "vault_paused": "0", "vault_queued_withdrawals": "0"})
}

func TestQueueFollowsTheEvents(t *testing.T) {
	c := newChain(20)
	c.balance, c.accounted = big.NewInt(100), big.NewInt(100)
	c.queue(12, 0, 1, 50, 72)
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{
		"vault_deployed": "1", "vault_block": "20", "vault_queued_withdrawals": "1", "vault_queued_wei": "50",
		"vault_queued_next_release_block": "72", "vault_withdrawals_queued_total": "1", "vault_solvent": "1",
		"vault_balance_wei": "100", "vault_accounted_wei": "100",
	})

	// A second one queued and the first paid, in one block, in log order.
	c.advance(5)
	c.queue(23, 0, 2, 30, 83)
	c.event(TopicPaid, 23, 1, 1)
	expect(t, metrics(t, w), map[string]string{
		"vault_queued_withdrawals": "1", "vault_queued_wei": "30", "vault_queued_next_release_block": "83",
		"vault_withdrawals_queued_total": "2", "vault_withdrawals_paid_total": "1",
	})

	c.advance(1)
	c.event(TopicCancelled, 26, 0, 2)
	expect(t, metrics(t, w), map[string]string{
		"vault_queued_withdrawals": "0", "vault_queued_wei": "0", "vault_queued_next_release_block": "0",
		"vault_withdrawals_cancelled_total": "1",
	})
}

func TestEventsBeforeTheDeploymentAreNotRead(t *testing.T) {
	c := newChain(20)
	c.queue(5, 0, 9, 1, 65) // block 5, before the record's block 10
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "0", "vault_withdrawals_queued_total": "0"})
}

func TestInsolventAndPausedAreReported(t *testing.T) {
	c := newChain(20)
	c.balance, c.accounted, c.paused = big.NewInt(99), big.NewInt(100), true
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_solvent": "0", "vault_paused": "1"})
}

func TestReorgRebuildsTheIndex(t *testing.T) {
	c := newChain(20)
	c.queue(15, 0, 1, 50, 75)
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "1"})

	// Block 20 is replaced, and the queue event with it: the index starts over.
	c.mu.Lock()
	c.hashes[20] = "0x" + strings.Repeat("f", 64)
	c.logs = nil
	c.mu.Unlock()
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "0", "vault_withdrawals_queued_total": "0"})
}

func TestAnotherVaultStartsOver(t *testing.T) {
	c := newChain(20)
	c.queue(15, 0, 1, 50, 75)
	w, path := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "1"})
	writeRecord(t, path, "0x00000000000000000000000000000000000000bb", 18)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "0", "vault_withdrawals_queued_total": "0"})
}

func TestLongHistoryIsReadInRanges(t *testing.T) {
	c := newChain(10 + 3*MaxRange)
	c.queue(10+2*MaxRange+5, 0, 1, 7, 10+2*MaxRange+65)
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "1"})
	c.mu.Lock()
	n := c.calls["eth_getLogs"]
	c.mu.Unlock()
	if want := 4; n != want { // blocks 10 .. 10+3*MaxRange, MaxRange at a time
		t.Fatalf("%d eth_getLogs calls, want %d", n, want)
	}
}

func TestAFailedPollKeepsTheLastStateAndCounts(t *testing.T) {
	c := newChain(20)
	c.queue(15, 0, 1, 50, 75)
	w, _ := setup(t, c, true)
	expect(t, metrics(t, w), map[string]string{"vault_queued_withdrawals": "1", "vault_watch_errors_total": "0"})
	c.mu.Lock()
	c.fail = true
	c.mu.Unlock()
	if err := w.Poll(context.Background()); err == nil {
		t.Fatal("poll against a failing node succeeded")
	}
	m := render(t, w)
	expect(t, m, map[string]string{"vault_queued_withdrawals": "1", "vault_watch_errors_total": "1"})
	if ts, _ := strconv.ParseInt(m["vault_watch_last_success_timestamp_seconds"], 10, 64); ts == 0 {
		t.Fatal("no time of the last good poll")
	}
}

func TestMalformedRecordIsAnError(t *testing.T) {
	c := newChain(20)
	w, path := setup(t, c, false)
	if err := os.WriteFile(path, []byte(`{"vault": "nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Poll(context.Background()); err == nil {
		t.Fatal("a record without a vault address was accepted")
	}
}
