// Package vaultwatch watches the vault on the L2 for what someone must see at once: a
// withdrawal waiting in the queue, the vault paused, the vault holding less ETH than it
// owes. It reads the chain as the verifier node derives it, keeps every queued
// withdrawal from the vault's events since its deployment, and renders the state as
// Prometheus metrics, which the alert rules read.
//
// The index is rebuilt from the deployment block when the block it last read is no
// longer in the chain (a reorganisation) or the deployment record names another vault.
package vaultwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event topics and call selectors of the vault (IVault), keccak256 of the signatures.
// cmd/vault-exporter's tests check them against the hashes go-ethereum computes.
const (
	TopicQueued       = "0x1a79633c28e7ce9c274b289a21239e281e380ae11083d0059214d4bedafa2e6e" // WithdrawalQueued(bytes32,address,address,uint256,uint64)
	TopicPaid         = "0x03688639eb6b7c429eef631b0e60ed9b7b03a8c3f0942390b52a94368e661ce2" // WithdrawalPaid(bytes32,address,address,uint256)
	TopicCancelled    = "0x9563b147ba38a395a14cca326c938ad69838052379ade073982c21fcdff7b321" // WithdrawalCancelled(bytes32,address)
	SelectorPaused    = "0x5c975abb"                                                         // paused()
	SelectorAccounted = "0xee1a56ce"                                                         // accounted(address)
	// Native is the address that stands for ETH in the vault (ERC-7528).
	Native = "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// MaxRange is the most blocks one eth_getLogs asks for.
const MaxRange = 2000

// Record is the part of the deployment record the watcher reads.
type Record struct {
	Vault string `json:"vault"`
	Block uint64 `json:"block"`
}

// ReadRecord reads the deployment record at path. A missing file is no error: there is
// no vault to watch yet, and ok is false.
func ReadRecord(path string) (r Record, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("vaultwatch: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, false, fmt.Errorf("vaultwatch: parse %s: %w", path, err)
	}
	if len(r.Vault) != 42 || !strings.HasPrefix(r.Vault, "0x") {
		return Record{}, false, fmt.Errorf("vaultwatch: %s: vault %q is not an address", path, r.Vault)
	}
	r.Vault = strings.ToLower(r.Vault)
	return r, true, nil
}

// queued is one withdrawal waiting in the queue.
type queued struct {
	amount    *big.Int
	releaseAt uint64
}

// State is what the watcher knows, as of Block.
type State struct {
	Deployed  bool
	Vault     string
	Block     uint64
	Paused    bool
	Balance   *big.Int
	Accounted *big.Int
	queue     map[string]queued
	Queued    uint64 // count of every WithdrawalQueued seen
	Paid      uint64
	Cancelled uint64
}

// Watcher keeps the state of one vault up to date.
type Watcher struct {
	Client     *Client
	RecordPath string

	mu       sync.Mutex
	state    State
	lastHash string
	errors   uint64
	lastOK   time.Time
}

// Poll reads what is new since the last poll. On an error it keeps the state it had
// and counts the error, so the metrics show the last good reading and that it is stale.
func (w *Watcher) Poll(ctx context.Context) error {
	err := w.poll(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.errors++
		return err
	}
	w.lastOK = time.Now()
	return nil
}

func (w *Watcher) poll(ctx context.Context) error {
	rec, ok, err := ReadRecord(w.RecordPath)
	if err != nil {
		return err
	}
	w.mu.Lock()
	s := w.state
	lastHash := w.lastHash
	w.mu.Unlock()

	if !ok {
		w.set(State{}, "")
		return nil
	}
	if !s.Deployed || s.Vault != rec.Vault {
		s = fresh(rec)
		lastHash = ""
	} else {
		s.queue = copyQueue(s.queue)
	}
	if lastHash != "" {
		h, err := w.Client.HeaderByNumber(ctx, s.Block, false)
		if err != nil {
			return err
		}
		if h.Hash != lastHash {
			s = fresh(rec)
		}
	}

	head, err := w.Client.HeaderByNumber(ctx, 0, true)
	if err != nil {
		return err
	}
	for from := s.Block + 1; from <= head.Number; from += MaxRange {
		to := min(from+MaxRange-1, head.Number)
		logs, err := w.Client.Logs(ctx, s.Vault, from, to, []string{TopicQueued, TopicPaid, TopicCancelled})
		if err != nil {
			return err
		}
		if err := s.apply(logs); err != nil {
			return err
		}
	}
	s.Block = head.Number

	out, err := w.Client.Call(ctx, s.Vault, SelectorPaused, head.Number)
	if err != nil {
		return err
	}
	paused, err := word(out, 0)
	if err != nil {
		return fmt.Errorf("vaultwatch: paused(): %w", err)
	}
	s.Paused = paused.Sign() != 0
	out, err = w.Client.Call(ctx, s.Vault, SelectorAccounted+strings.Repeat("0", 24)+Native[2:], head.Number)
	if err != nil {
		return err
	}
	if s.Accounted, err = word(out, 0); err != nil {
		return fmt.Errorf("vaultwatch: accounted(): %w", err)
	}
	if s.Balance, err = w.Client.Balance(ctx, s.Vault, head.Number); err != nil {
		return err
	}
	w.set(s, head.Hash)
	return nil
}

// fresh is the state before the vault's deployment block: nothing read yet.
func fresh(rec Record) State {
	before := uint64(0)
	if rec.Block > 0 {
		before = rec.Block - 1
	}
	return State{Deployed: true, Vault: rec.Vault, Block: before, queue: map[string]queued{}}
}

func (w *Watcher) set(s State, hash string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = s
	w.lastHash = hash
}

// apply folds logs, in chain order, into the queue.
func (s *State) apply(logs []Log) error {
	type ordered struct {
		block, index uint64
		log          Log
	}
	list := make([]ordered, 0, len(logs))
	for _, l := range logs {
		if l.Removed || !strings.EqualFold(l.Address, s.Vault) {
			continue
		}
		b, err := parseQuantity(l.BlockNumber)
		if err != nil {
			return err
		}
		i, err := parseQuantity(l.LogIndex)
		if err != nil {
			return err
		}
		list = append(list, ordered{b, i, l})
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].block != list[b].block {
			return list[a].block < list[b].block
		}
		return list[a].index < list[b].index
	})
	for _, o := range list {
		l := o.log
		if len(l.Topics) < 2 {
			return fmt.Errorf("vaultwatch: log in block %d has no id", o.block)
		}
		id := strings.ToLower(l.Topics[1])
		switch strings.ToLower(l.Topics[0]) {
		case TopicQueued:
			data, err := decodeHex(l.Data)
			if err != nil {
				return err
			}
			amount, err := word(data, 0)
			if err != nil {
				return fmt.Errorf("vaultwatch: WithdrawalQueued: %w", err)
			}
			release, err := word(data, 1)
			if err != nil || !release.IsUint64() {
				return fmt.Errorf("vaultwatch: WithdrawalQueued: release block: %w", errors.Join(err, errors.New("not a uint64")))
			}
			s.queue[id] = queued{amount: amount, releaseAt: release.Uint64()}
			s.Queued++
		case TopicPaid:
			delete(s.queue, id)
			s.Paid++
		case TopicCancelled:
			delete(s.queue, id)
			s.Cancelled++
		}
	}
	return nil
}

func copyQueue(q map[string]queued) map[string]queued {
	out := make(map[string]queued, len(q))
	for k, v := range q {
		out[k] = v
	}
	return out
}

// word returns the i-th 32-byte word of data as an unsigned integer.
func word(data []byte, i int) (*big.Int, error) {
	if len(data) < 32*(i+1) {
		return nil, fmt.Errorf("%d bytes, want at least %d", len(data), 32*(i+1))
	}
	return new(big.Int).SetBytes(data[32*i : 32*(i+1)]), nil
}

// WriteMetrics writes the state in the Prometheus text format.
func (w *Watcher) WriteMetrics(out io.Writer) error {
	w.mu.Lock()
	s := w.state
	errs := w.errors
	lastOK := w.lastOK
	w.mu.Unlock()

	var b strings.Builder
	gauge := func(name, help, value string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, value)
	}
	counter := func(name, help string, value uint64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
	}
	flag := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	// With no vault deployed there is nothing to watch: the vault is reported solvent,
	// unpaused and with an empty queue, so no alert fires, and vault_deployed says why.
	pending, pendingWei := uint64(0), new(big.Int)
	nextRelease := uint64(0)
	for _, q := range s.queue {
		pending++
		pendingWei.Add(pendingWei, q.amount)
		if nextRelease == 0 || q.releaseAt < nextRelease {
			nextRelease = q.releaseAt
		}
	}
	solvent := true
	if s.Deployed && s.Balance != nil && s.Accounted != nil {
		solvent = s.Balance.Cmp(s.Accounted) >= 0
	}
	gauge("vault_deployed", "1 when a deployment record names a vault to watch.", flag(s.Deployed))
	gauge("vault_block", "The last L2 block read.", strconv.FormatUint(s.Block, 10))
	gauge("vault_paused", "1 while the vault is paused.", flag(s.Paused))
	gauge("vault_solvent", "1 while the vault holds at least the ETH it accounts for, compared exactly in wei.", flag(solvent))
	gauge("vault_balance_wei", "ETH the vault holds, in wei.", bigOrZero(s.Balance))
	gauge("vault_accounted_wei", "ETH the vault owes to deposits, in wei.", bigOrZero(s.Accounted))
	gauge("vault_queued_withdrawals", "Withdrawals waiting in the queue, neither paid nor cancelled.", strconv.FormatUint(pending, 10))
	gauge("vault_queued_wei", "Wei waiting in the queue.", pendingWei.String())
	gauge("vault_queued_next_release_block", "The earliest release block in the queue; 0 when it is empty.", strconv.FormatUint(nextRelease, 10))
	counter("vault_withdrawals_queued_total", "WithdrawalQueued events seen.", s.Queued)
	counter("vault_withdrawals_paid_total", "WithdrawalPaid events seen.", s.Paid)
	counter("vault_withdrawals_cancelled_total", "WithdrawalCancelled events seen.", s.Cancelled)
	counter("vault_watch_errors_total", "Polls that failed; the state shown is the last good one.", errs)
	last := "0"
	if !lastOK.IsZero() {
		last = strconv.FormatInt(lastOK.Unix(), 10)
	}
	gauge("vault_watch_last_success_timestamp_seconds", "When a poll last succeeded.", last)
	_, err := io.WriteString(out, b.String())
	return err
}

func bigOrZero(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}
