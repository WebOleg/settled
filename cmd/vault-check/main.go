// Command vault-check runs the vault through its rules on the devnet L2 with real
// transactions, from the keys that hold each role, and accounts for every wei:
//
//   - the devnet user deposits ETH for an exchange account;
//   - the operator pays a small withdrawal at once, and the same id is refused after;
//   - the operator asks for a large withdrawal, which is queued, and refused before its
//     release block;
//   - the guardian pauses the vault: deposits and payments are refused, and the
//     guardian cannot unpause; the admin unpauses it through the timelock;
//   - a change the proposer schedules on the timelock is stopped by the canceller, and
//     the proposer cannot cancel it itself;
//   - after the release block the operator pays the queued withdrawal;
//   - the vault's balance, what it accounts for, and what the recipient received are
//     exact to the wei;
//   - the monitoring sees it: VaultWithdrawalQueued and VaultPaused fire and reach
//     Alertmanager while the withdrawal waits and the vault is paused, and clear after.
//
// Every run uses new ids and a new recipient, derived from the block it starts at, so
// runs never collide. With -preflight it only checks that a run can start, and sends
// nothing. A vault found paused, which a run stopped midway leaves, is unpaused through
// the timelock first.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/timelockop"
	"github.com/WebOleg/settled/internal/txsend"
	"github.com/WebOleg/settled/internal/vaultconf"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// UserIndex is the devnet user, who holds ETH on the L2 and pays the keys' gas.
	UserIndex = 2
	// Withdrawal status values, as IVault.WithdrawalStatus numbers them.
	statusQueued = 1
	statusPaid   = 2
)

// gasReserve is what the user keeps beyond the deposit, for its own gas and the keys'.
var gasReserve = new(big.Int).Div(funding.Ether(1), big.NewInt(10)) // 0.1 ETH

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	preflight := flag.Bool("preflight", false, "only check that a run can start; send nothing")
	prometheus := flag.String("prometheus", "http://127.0.0.1:9090", "Prometheus, which evaluates the vault's alerts")
	alertmanager := flag.String("alertmanager", "http://127.0.0.1:9093", "Alertmanager, which receives them")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	mon := &monitor{prometheus: *prometheus, alertmanager: *alertmanager, http: &http.Client{Timeout: 5 * time.Second}}
	if err := run(ctx, *rpcURL, *outDir, *preflight, mon); err != nil {
		fmt.Fprintf(os.Stderr, "vault-check: FAIL: %v\n", err)
		os.Exit(1)
	}
}

// checker holds what every step needs.
type checker struct {
	ctx      context.Context
	client   *ethclient.Client
	vault    *artifact.Artifact
	timelock *artifact.Artifact
	d        *vaultconf.Deployment
}

func ok(name, detail string) {
	fmt.Printf("ok    %-52s %s\n", name, detail)
}

func run(ctx context.Context, rpcURL, outDir string, preflight bool, mon *monitor) error {
	d, err := vaultconf.ReadDeployment(".")
	if err != nil {
		return fmt.Errorf("%w (run make vault-deploy)", err)
	}
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer client.Close()
	genesis, err := client.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return errors.New("the deployment record belongs to another L2; run make vault-reset and make vault-deploy")
	}
	vault, err := artifact.Load(outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	timelock, err := artifact.Load(outDir, "TimelockController.sol", "TimelockController")
	if err != nil {
		return err
	}
	c := &checker{ctx: ctx, client: client, vault: vault, timelock: timelock, d: d}

	keys := map[rolekeys.Role]*ecdsa.PrivateKey{}
	for _, r := range []rolekeys.Role{vaultconf.Operator, vaultconf.Guardian, vaultconf.TimelockProposer, vaultconf.TimelockCanceller} {
		if keys[r], err = rolekeys.Load(vaultconf.KeysDir, r); err != nil {
			return err
		}
	}
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return fmt.Errorf("%s is not set", mnemonicEnv)
	}
	user, err := devkeys.FromMnemonic(mnemonic, UserIndex)
	if err != nil {
		return fmt.Errorf("derive the devnet user key: %w", err)
	}
	userAddr := crypto.PubkeyToAddress(user.PublicKey)
	operator := crypto.PubkeyToAddress(keys[vaultconf.Operator].PublicKey)
	guardian := crypto.PubkeyToAddress(keys[vaultconf.Guardian].PublicKey)

	// The amounts follow the ETH configuration: a large withdrawal is exactly the queue
	// threshold, a small one 1 ETH, and the deposit covers both, so the books end where
	// they started.
	config := vaultconf.NativeConfig()
	small := funding.Ether(1)
	large := new(big.Int).Set(config.LargeThreshold)
	deposit := new(big.Int).Add(small, large)

	head, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("read the head: %w", err)
	}
	userBalance, err := client.BalanceAt(ctx, userAddr, nil)
	if err != nil {
		return fmt.Errorf("read the user's balance: %w", err)
	}
	need := new(big.Int).Add(deposit, gasReserve)
	if userBalance.Cmp(need) < 0 {
		short := new(big.Int).Sub(need, userBalance)
		return fmt.Errorf("the devnet user %s holds %s ETH on the L2 and needs %s ETH: %s ETH short (make l2-deposit brings ETH from the L1)",
			userAddr, funding.FormatEther(userBalance), funding.FormatEther(need), funding.FormatEther(short))
	}
	ok("devnet user can fund the run", fmt.Sprintf("%s ETH, needs %s", funding.FormatEther(userBalance), funding.FormatEther(need)))
	paused, err := c.number("paused()", nil)
	if err != nil {
		return err
	}
	available, err := c.number("available(address)", nil, chaincall.AddressWord(vaultconf.Native))
	if err != nil {
		return err
	}
	if paused.Sign() == 0 && available.Cmp(small) < 0 {
		return fmt.Errorf("the rate limit lets out %s ETH, less than %s; wait for it to refill", funding.FormatEther(available), funding.FormatEther(small))
	}
	if paused.Sign() == 0 {
		// Before a run, no vault alert may fire, or the run could not show its own.
		for _, a := range []string{alertQueued, alertPaused} {
			if firing, err := mon.firing(ctx, a); err != nil {
				return fmt.Errorf("%w (make obs-up starts the monitoring)", err)
			} else if firing {
				return fmt.Errorf("%s is firing before the run; see docs/runbooks.md", a)
			}
		}
		ok("monitoring answers, no vault alert firing", mon.prometheus)
	}
	// The cancel step needs a timelock whose proposer cannot cancel.
	proposerAddr := crypto.PubkeyToAddress(keys[vaultconf.TimelockProposer].PublicKey)
	cancellerAddr := crypto.PubkeyToAddress(keys[vaultconf.TimelockCanceller].PublicKey)
	proposerCancels, err := c.timelockWord("hasRole", timelockop.CancellerRole, proposerAddr)
	if err != nil {
		return err
	}
	cancellerCancels, err := c.timelockWord("hasRole", timelockop.CancellerRole, cancellerAddr)
	if err != nil {
		return err
	}
	if proposerCancels != (common.Hash{}) || cancellerCancels == (common.Hash{}) {
		return fmt.Errorf("the timelock %s does not have its canceller apart from its proposer (make vault-timelock-move)", d.Timelock)
	}
	ok("timelock canceller is apart from the proposer", d.Timelock.Hex())
	if preflight {
		fmt.Printf("vault-check: preflight ok; vault %s, block %s, paused %v\n", d.Vault, head.Number, paused.Sign() != 0)
		return nil
	}

	// Gas for the keys that send transactions, from the user.
	targets := []funding.Target{}
	for _, r := range []rolekeys.Role{vaultconf.Operator, vaultconf.Guardian, vaultconf.TimelockProposer, vaultconf.TimelockCanceller} {
		a := crypto.PubkeyToAddress(keys[r].PublicKey)
		targets = append(targets, funding.Target{Name: string(r), Address: a, Balance: new(big.Int).Div(funding.Ether(1), big.NewInt(50))})
	}
	if _, err := funding.TopUp(ctx, client, user, targets); err != nil {
		return fmt.Errorf("top up the keys: %w", err)
	}

	if paused.Sign() != 0 {
		fmt.Println("recover          the vault is paused, as a stopped run leaves it; unpausing through the timelock")
		if err := c.unpause(keys[vaultconf.TimelockProposer], "recover "+head.Number.String()); err != nil {
			return err
		}
	}

	// Names of this run, from the block it starts at.
	tag := "settled: vault-check " + head.Number.String()
	var account, idSmall, idLarge [32]byte = crypto.Keccak256Hash([]byte(tag + " account")),
		crypto.Keccak256Hash([]byte(tag + " small")), crypto.Keccak256Hash([]byte(tag + " large"))
	recipient := common.BytesToAddress(crypto.Keccak256([]byte(tag + " recipient"))[12:])
	fmt.Printf("run              from block %s; recipient %s\n", head.Number, recipient)

	vaultBefore, err := client.BalanceAt(ctx, d.Vault, nil)
	if err != nil {
		return fmt.Errorf("read the vault's balance: %w", err)
	}
	owedBefore, err := c.number("accounted(address)", nil, chaincall.AddressWord(vaultconf.Native))
	if err != nil {
		return err
	}
	countBefore, err := c.number("depositCount()", nil)
	if err != nil {
		return err
	}
	if b, err := client.BalanceAt(ctx, recipient, nil); err != nil || b.Sign() != 0 {
		return fmt.Errorf("the recipient %s must start empty: %v, %v", recipient, b, err)
	}

	// 1. Deposit.
	data, err := vault.Call("deposit", vaultconf.Native, deposit, account)
	if err != nil {
		return err
	}
	receipt, err := txsend.Send(ctx, client, user, d.Vault, deposit, data)
	if err != nil {
		return fmt.Errorf("deposit: %w", err)
	}
	topics, values, err := c.event(receipt, "Deposited")
	if err != nil {
		return err
	}
	if err := all(
		equalWord("deposit id", topics[1], common.BigToHash(countBefore)),
		equalWord("token", topics[2], common.BytesToHash(vaultconf.Native.Bytes())),
		equalWord("from", topics[3], common.BytesToHash(userAddr.Bytes())),
		equalValue("account", values[0], account),
		equalAmount("amount", values[1], deposit),
	); err != nil {
		return fmt.Errorf("the Deposited event: %w", err)
	}
	ok("deposit credited, with the next deposit id", fmt.Sprintf("id %s, %s ETH, block %s", countBefore, funding.FormatEther(deposit), receipt.BlockNumber))
	if err := c.books("books after the deposit", vaultBefore, owedBefore, deposit, nil); err != nil {
		return err
	}

	// 2. A small withdrawal, paid at once.
	data, err = vault.Call("withdraw", idSmall, vaultconf.Native, recipient, small)
	if err != nil {
		return err
	}
	receipt, err = txsend.Send(ctx, client, keys[vaultconf.Operator], d.Vault, new(big.Int), data)
	if err != nil {
		return fmt.Errorf("small withdrawal: %w", err)
	}
	if err := c.paid(receipt, idSmall, recipient, small); err != nil {
		return err
	}
	if err := c.status(idSmall, statusPaid); err != nil {
		return err
	}
	ok("small withdrawal paid at once", fmt.Sprintf("%s ETH, block %s", funding.FormatEther(small), receipt.BlockNumber))
	if err := c.rateLimitTook(receipt.BlockNumber, small, config.Capacity); err != nil {
		return err
	}
	if err := c.balance("recipient received exactly the amount", recipient, small); err != nil {
		return err
	}
	if err := c.books("books after the small withdrawal", vaultBefore, owedBefore, deposit, small); err != nil {
		return err
	}

	// 3. The same id again.
	if err := c.refused(operator, data, nil, "WithdrawalExists", idSmall, uint8(statusPaid)); err != nil {
		return fmt.Errorf("a paid id used again: %w", err)
	}
	ok("a paid id is refused", "WithdrawalExists(id, Paid)")

	// A change no one wants, scheduled with the proposer's key as a thief would: the
	// proposer cannot cancel it, the canceller does, and it is gone from the timelock.
	if err := c.cancelled(keys[vaultconf.TimelockProposer], keys[vaultconf.TimelockCanceller], tag); err != nil {
		return err
	}

	// 4. A large withdrawal, queued.
	data, err = vault.Call("withdraw", idLarge, vaultconf.Native, recipient, large)
	if err != nil {
		return err
	}
	receipt, err = txsend.Send(ctx, client, keys[vaultconf.Operator], d.Vault, new(big.Int), data)
	if err != nil {
		return fmt.Errorf("large withdrawal: %w", err)
	}
	topics, values, err = c.event(receipt, "WithdrawalQueued")
	if err != nil {
		return err
	}
	releaseAt := receipt.BlockNumber.Uint64() + vaultconf.WithdrawalDelay
	if err := all(
		equalWord("id", topics[1], idLarge),
		equalWord("to", topics[3], common.BytesToHash(recipient.Bytes())),
		equalAmount("amount", values[0], large),
		equalValue("release block", values[1], releaseAt),
	); err != nil {
		return fmt.Errorf("the WithdrawalQueued event: %w", err)
	}
	if err := c.status(idLarge, statusQueued); err != nil {
		return err
	}
	ok("large withdrawal queued, not paid", fmt.Sprintf("%s ETH, block %s, release block %d", funding.FormatEther(large), receipt.BlockNumber, releaseAt))
	if err := c.balance("recipient still holds only the small amount", recipient, small); err != nil {
		return err
	}
	execute, err := vault.Call("executeQueued", idLarge)
	if err != nil {
		return err
	}
	if err := c.refused(operator, execute, nil, "WithdrawalNotReleased", idLarge, releaseAt); err != nil {
		return fmt.Errorf("a queued withdrawal before its release: %w", err)
	}
	ok("the queue is refused before the release block", fmt.Sprintf("WithdrawalNotReleased(id, %d)", releaseAt))
	if err := c.alert(mon, alertQueued, true); err != nil {
		return err
	}

	// 5. Pause.
	pause, err := vault.Call("pause")
	if err != nil {
		return err
	}
	receipt, err = txsend.Send(ctx, client, keys[vaultconf.Guardian], d.Vault, new(big.Int), pause)
	if err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if p, err := c.number("paused()", nil); err != nil || p.Sign() == 0 {
		return fmt.Errorf("not paused after the guardian's pause: %v", err)
	}
	ok("guardian paused the vault", fmt.Sprintf("block %s", receipt.BlockNumber))
	if err := c.alert(mon, alertPaused, true); err != nil {
		return err
	}
	depositAgain, err := vault.Call("deposit", vaultconf.Native, small, account)
	if err != nil {
		return err
	}
	if err := c.refused(userAddr, depositAgain, small, "EnforcedPause"); err != nil {
		return fmt.Errorf("a deposit while paused: %w", err)
	}
	ok("paused: deposits are refused", "EnforcedPause()")
	if err := c.refused(operator, execute, nil, "EnforcedPause"); err != nil {
		return fmt.Errorf("a payment while paused: %w", err)
	}
	ok("paused: payments are refused", "EnforcedPause()")
	unpause, err := vault.Call("unpause")
	if err != nil {
		return err
	}
	if err := c.refused(guardian, unpause, nil, "AccessControlUnauthorizedAccount", guardian, [32]byte(vaultconf.AdminRole)); err != nil {
		return fmt.Errorf("the guardian unpausing: %w", err)
	}
	ok("the guardian cannot unpause", "AccessControlUnauthorizedAccount(guardian, DEFAULT_ADMIN_ROLE)")

	// 6. Unpause, through the timelock.
	if err := c.unpause(keys[vaultconf.TimelockProposer], tag); err != nil {
		return err
	}
	if err := c.alert(mon, alertPaused, false); err != nil {
		return err
	}

	// 7. The queued withdrawal, after its release block.
	if err := c.waitForBlock(releaseAt); err != nil {
		return err
	}
	receipt, err = txsend.Send(ctx, client, keys[vaultconf.Operator], d.Vault, new(big.Int), execute)
	if err != nil {
		return fmt.Errorf("execute the queued withdrawal: %w", err)
	}
	if receipt.BlockNumber.Uint64() < releaseAt {
		return fmt.Errorf("paid in block %s, before the release block %d", receipt.BlockNumber, releaseAt)
	}
	if err := c.paid(receipt, idLarge, recipient, large); err != nil {
		return err
	}
	if err := c.status(idLarge, statusPaid); err != nil {
		return err
	}
	ok("queued withdrawal paid after its release block", fmt.Sprintf("%s ETH, block %s", funding.FormatEther(large), receipt.BlockNumber))
	if err := c.alert(mon, alertQueued, false); err != nil {
		return err
	}

	// 8. Every wei.
	if err := c.balance("recipient received exactly both amounts", recipient, deposit); err != nil {
		return err
	}
	if err := c.books("books back where they started", vaultBefore, owedBefore, deposit, deposit); err != nil {
		return err
	}
	count, err := c.number("depositCount()", nil)
	if err != nil {
		return err
	}
	if want := new(big.Int).Add(countBefore, big.NewInt(1)); count.Cmp(want) != 0 {
		return fmt.Errorf("depositCount() is %s, want %s", count, want)
	}
	ok("one deposit counted", count.String())
	fmt.Println("vault-check: ok")
	return nil
}

// The vault's alerts (infra/observability/alerts.yml) that a run makes fire.
const (
	alertQueued = "VaultWithdrawalQueued"
	alertPaused = "VaultPaused"
	// alertWait is how long an alert may take to follow the chain: the exporter polls
	// every 2 s, Prometheus scrapes and evaluates every 5 s, and Alertmanager waits 10 s
	// before it groups a new alert.
	alertWait = 60 * time.Second
)

// monitor asks Prometheus and Alertmanager about alerts.
type monitor struct {
	prometheus, alertmanager string
	http                     *http.Client
}

// firing reports whether Prometheus has the alert firing.
func (m *monitor) firing(ctx context.Context, name string) (bool, error) {
	var out struct {
		Data struct {
			Alerts []struct {
				Labels map[string]string `json:"labels"`
				State  string            `json:"state"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := m.get(ctx, m.prometheus+"/api/v1/alerts", &out); err != nil {
		return false, err
	}
	for _, a := range out.Data.Alerts {
		if a.Labels["alertname"] == name && a.State == "firing" {
			return true, nil
		}
	}
	return false, nil
}

// received reports whether Alertmanager holds the alert as active.
func (m *monitor) received(ctx context.Context, name string) (bool, error) {
	var out []struct {
		Labels map[string]string `json:"labels"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := m.get(ctx, m.alertmanager+"/api/v2/alerts", &out); err != nil {
		return false, err
	}
	for _, a := range out {
		if a.Labels["alertname"] == name && a.Status.State == "active" {
			return true, nil
		}
	}
	return false, nil
}

func (m *monitor) get(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("monitoring: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("monitoring: %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// alert waits until the alert fires in Prometheus and is active in Alertmanager (want
// true), or no longer fires in Prometheus (want false).
func (c *checker) alert(mon *monitor, name string, want bool) error {
	start := time.Now()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		firing, err := mon.firing(c.ctx, name)
		if err != nil {
			return err
		}
		done := firing == want
		if done && want {
			if done, err = mon.received(c.ctx, name); err != nil {
				return err
			}
		}
		if done {
			state := "fires and reached Alertmanager"
			if !want {
				state = "cleared"
			}
			ok(fmt.Sprintf("alert %s %s", name, state), fmt.Sprintf("after %d s", int(time.Since(start).Seconds())))
			return nil
		}
		if time.Since(start) > alertWait {
			return fmt.Errorf("alert %s: want firing %v, still not after %s", name, want, alertWait)
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-tick.C:
		}
	}
}

// unpause runs unpause() through the timelock and checks the vault is no longer paused.
func (c *checker) unpause(proposer *ecdsa.PrivateKey, salt string) error {
	call, err := c.vault.Call("unpause")
	if err != nil {
		return err
	}
	op := timelockop.Op{Target: c.d.Vault, Data: call, Salt: crypto.Keccak256Hash([]byte(salt + " unpause"))}
	receipt, _, err := timelockop.Run(c.ctx, c.client, proposer, c.timelock, c.d.Timelock, op, big.NewInt(vaultconf.TimelockDelay), func(s string) {
		fmt.Printf("proposed         unpause: %s\n", s)
	})
	if err != nil {
		return fmt.Errorf("unpause through the timelock: %w", err)
	}
	if p, err := c.number("paused()", nil); err != nil || p.Sign() != 0 {
		return fmt.Errorf("still paused after the timelock's unpause: %v", err)
	}
	ok("admin unpaused through the timelock", fmt.Sprintf("block %s", receipt.BlockNumber))
	return nil
}

// paid checks the WithdrawalPaid event of receipt.
func (c *checker) paid(receipt *types.Receipt, id [32]byte, to common.Address, amount *big.Int) error {
	topics, values, err := c.event(receipt, "WithdrawalPaid")
	if err != nil {
		return err
	}
	if err := all(
		equalWord("id", topics[1], id),
		equalWord("token", topics[2], common.BytesToHash(vaultconf.Native.Bytes())),
		equalWord("to", topics[3], common.BytesToHash(to.Bytes())),
		equalAmount("amount", values[0], amount),
	); err != nil {
		return fmt.Errorf("the WithdrawalPaid event: %w", err)
	}
	return nil
}

// books checks the vault's balance and what it owes against the start of the run: both
// rose by deposited and fell by paid (nil for nothing).
func (c *checker) books(name string, vaultBefore, owedBefore, deposited, paid *big.Int) error {
	delta := new(big.Int).Set(deposited)
	if paid != nil {
		delta.Sub(delta, paid)
	}
	balance, err := c.client.BalanceAt(c.ctx, c.d.Vault, nil)
	if err != nil {
		return fmt.Errorf("read the vault's balance: %w", err)
	}
	owed, err := c.number("accounted(address)", nil, chaincall.AddressWord(vaultconf.Native))
	if err != nil {
		return err
	}
	wantBalance := new(big.Int).Add(vaultBefore, delta)
	wantOwed := new(big.Int).Add(owedBefore, delta)
	if balance.Cmp(wantBalance) != 0 || owed.Cmp(wantOwed) != 0 {
		return fmt.Errorf("%s: balance %s wei and accounted %s wei, want %s and %s", name, balance, owed, wantBalance, wantOwed)
	}
	if balance.Cmp(owed) < 0 {
		return fmt.Errorf("%s: the vault holds %s wei, less than the %s it owes", name, balance, owed)
	}
	ok(name, fmt.Sprintf("balance %s wei, accounted %s wei", balance, owed))
	return nil
}

// rateLimitTook checks that a payment in block took exactly amount from a full rate
// limit: the limit is read just before and in the payment's block.
func (c *checker) rateLimitTook(block, amount, capacity *big.Int) error {
	before := new(big.Int).Sub(block, big.NewInt(1))
	arg := chaincall.AddressWord(vaultconf.Native)
	a0, err := c.number("available(address)", before, arg)
	if err != nil {
		return err
	}
	a1, err := c.number("available(address)", block, arg)
	if err != nil {
		return err
	}
	if a0.Cmp(capacity) != 0 {
		ok("rate limit (not full before, so not compared)", fmt.Sprintf("%s ETH before, %s after", funding.FormatEther(a0), funding.FormatEther(a1)))
		return nil
	}
	if want := new(big.Int).Sub(capacity, amount); a1.Cmp(want) != 0 {
		return fmt.Errorf("the rate limit holds %s wei after the payment, want %s", a1, want)
	}
	ok("rate limit took exactly the amount", fmt.Sprintf("%s ETH of %s left", funding.FormatEther(a1), funding.FormatEther(capacity)))
	return nil
}

// status checks withdrawal(id).status.
func (c *checker) status(id [32]byte, want uint64) error {
	w, err := c.words("withdrawal(bytes32)", nil, common.Hash(id))
	if err != nil {
		return err
	}
	if len(w) != 5 {
		return fmt.Errorf("withdrawal(): %d words, want 5", len(w))
	}
	if got := chaincall.UintOf(w[0]); got.Cmp(new(big.Int).SetUint64(want)) != 0 {
		return fmt.Errorf("withdrawal %x has status %s, want %d", id, got, want)
	}
	return nil
}

// balance checks the ETH balance of account.
func (c *checker) balance(name string, account common.Address, want *big.Int) error {
	got, err := c.client.BalanceAt(c.ctx, account, nil)
	if err != nil {
		return fmt.Errorf("read the balance of %s: %w", account, err)
	}
	if got.Cmp(want) != 0 {
		return fmt.Errorf("%s: %s holds %s wei, want %s", name, account, got, want)
	}
	ok(name, fmt.Sprintf("%s wei", got))
	return nil
}

// waitForBlock waits until the L2 head reaches block.
func (c *checker) waitForBlock(block uint64) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		n, err := c.client.BlockNumber(c.ctx)
		if err != nil {
			return fmt.Errorf("read the block number: %w", err)
		}
		if n >= block {
			return nil
		}
		select {
		case <-c.ctx.Done():
			return fmt.Errorf("block %d not reached: %w", block, c.ctx.Err())
		case <-tick.C:
		}
	}
}

// cancelled schedules a change on the timelock with the proposer's key, shows that the
// proposer cannot cancel it, cancels it with the canceller's key and checks it is gone.
func (c *checker) cancelled(proposer, canceller *ecdsa.PrivateKey, tag string) error {
	// The longest queue delay the vault takes (MAX_WITHDRAWAL_DELAY, 30 days): it would
	// hold every large withdrawal for a month.
	change, err := c.vault.Call("setWithdrawalDelay", uint64(1_296_000))
	if err != nil {
		return err
	}
	salt, none := crypto.Keccak256Hash([]byte(tag+" unwanted change")), common.Hash{}
	delay := big.NewInt(vaultconf.TimelockDelay)
	schedule, err := c.timelock.Call("schedule", c.d.Vault, new(big.Int), change, none, salt, delay)
	if err != nil {
		return err
	}
	receipt, err := txsend.Send(c.ctx, c.client, proposer, c.d.Timelock, new(big.Int), schedule)
	if err != nil {
		return fmt.Errorf("schedule the unwanted change: %w", err)
	}
	id, err := c.timelockWord("hashOperation", c.d.Vault, new(big.Int), change, none, salt)
	if err != nil {
		return err
	}
	if w, err := c.timelockWord("isOperationPending", id); err != nil {
		return err
	} else if w != common.BigToHash(big.NewInt(1)) {
		return fmt.Errorf("the unwanted change %x is not pending", id)
	}
	ok("proposer scheduled an unwanted change", fmt.Sprintf("operation %x, block %s", id, receipt.BlockNumber))
	cancel, err := c.timelock.Call("cancel", id)
	if err != nil {
		return err
	}
	proposerAddr := crypto.PubkeyToAddress(proposer.PublicKey)
	if err := c.refusedAt(c.timelock, c.d.Timelock, proposerAddr, cancel, nil, "AccessControlUnauthorizedAccount", proposerAddr, [32]byte(timelockop.CancellerRole)); err != nil {
		return fmt.Errorf("the proposer cancelling: %w", err)
	}
	ok("the proposer cannot cancel it", "AccessControlUnauthorizedAccount(proposer, CANCELLER_ROLE)")
	receipt, err = txsend.Send(c.ctx, c.client, canceller, c.d.Timelock, new(big.Int), cancel)
	if err != nil {
		return fmt.Errorf("cancel with the canceller's key: %w", err)
	}
	if w, err := c.timelockWord("isOperation", id); err != nil {
		return err
	} else if w != (common.Hash{}) {
		return fmt.Errorf("the change %x is still in the timelock after the cancel", id)
	}
	ok("the canceller cancelled it; it is gone", fmt.Sprintf("block %s", receipt.BlockNumber))
	return nil
}

// timelockWord calls a view of the timelock that returns one word.
func (c *checker) timelockWord(method string, args ...any) ([32]byte, error) {
	data, err := c.timelock.Call(method, args...)
	if err != nil {
		return [32]byte{}, err
	}
	out, err := c.client.CallContract(c.ctx, ethereum.CallMsg{To: &c.d.Timelock, Data: data}, nil)
	if err != nil {
		return [32]byte{}, fmt.Errorf("timelock %s: %w", method, err)
	}
	if len(out) != 32 {
		return [32]byte{}, fmt.Errorf("timelock %s: %d bytes, want 32", method, len(out))
	}
	return [32]byte(out), nil
}

// refused checks that the vault refuses data from `from` with exactly the custom error
// errName and args, by calling it without sending a transaction.
func (c *checker) refused(from common.Address, data []byte, value *big.Int, errName string, args ...any) error {
	return c.refusedAt(c.vault, c.d.Vault, from, data, value, errName, args...)
}

// refusedAt is refused for any contract: the error is looked up in its ABI.
func (c *checker) refusedAt(a *artifact.Artifact, to common.Address, from common.Address, data []byte, value *big.Int, errName string, args ...any) error {
	e, found := a.ABI.Errors[errName]
	if !found {
		return fmt.Errorf("no error %s in the ABI of %s", errName, a.Name)
	}
	packed, err := e.Inputs.Pack(args...)
	if err != nil {
		return fmt.Errorf("encode %s: %w", errName, err)
	}
	want := "0x" + hex.EncodeToString(append(append([]byte{}, e.ID[:4]...), packed...))
	_, err = c.client.CallContract(c.ctx, ethereum.CallMsg{From: from, To: &to, Value: value, Data: data}, nil)
	if err == nil {
		return fmt.Errorf("the call succeeds; want %s", errName)
	}
	var de rpc.DataError
	if !errors.As(err, &de) {
		return fmt.Errorf("refused without revert data: %w", err)
	}
	got, _ := de.ErrorData().(string)
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("refused with %s, want %s %s", got, errName, want)
	}
	return nil
}

// event finds the first log of the vault named name in receipt and returns its topics
// and its non-indexed values.
func (c *checker) event(receipt *types.Receipt, name string) ([]common.Hash, []any, error) {
	ev, found := c.vault.ABI.Events[name]
	if !found {
		return nil, nil, fmt.Errorf("no event %s in the vault's ABI", name)
	}
	for _, l := range receipt.Logs {
		if l.Address != c.d.Vault || len(l.Topics) != 4 || l.Topics[0] != ev.ID {
			continue
		}
		values, err := ev.Inputs.NonIndexed().Unpack(l.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", name, err)
		}
		return l.Topics, values, nil
	}
	return nil, nil, fmt.Errorf("no %s event in %s", name, receipt.TxHash)
}

// number calls a view that returns one unsigned integer, in block (nil for the latest).
func (c *checker) number(signature string, block *big.Int, args ...common.Hash) (*big.Int, error) {
	w, err := c.words(signature, block, args...)
	if err != nil {
		return nil, err
	}
	if len(w) != 1 {
		return nil, fmt.Errorf("%s: %d words, want 1", signature, len(w))
	}
	return chaincall.UintOf(w[0]), nil
}

// words calls a view of the vault whose arguments are each one word, in block.
func (c *checker) words(signature string, block *big.Int, args ...common.Hash) ([]common.Hash, error) {
	data := chaincall.Selector(signature)
	for _, a := range args {
		data = append(data, a.Bytes()...)
	}
	out, err := c.client.CallContract(c.ctx, ethereum.CallMsg{To: &c.d.Vault, Data: data}, block)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", signature, err)
	}
	if len(out) == 0 || len(out)%32 != 0 {
		return nil, fmt.Errorf("%s: %d bytes returned", signature, len(out))
	}
	w := make([]common.Hash, len(out)/32)
	for i := range w {
		w[i] = common.BytesToHash(out[32*i : 32*(i+1)])
	}
	return w, nil
}

// all returns the first error.
func all(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func equalWord(name string, got common.Hash, want [32]byte) error {
	if got != common.Hash(want) {
		return fmt.Errorf("%s is %s, want %x", name, got, want)
	}
	return nil
}

func equalAmount(name string, got any, want *big.Int) error {
	v, isInt := got.(*big.Int)
	if !isInt || v.Cmp(want) != 0 {
		return fmt.Errorf("%s is %v, want %s", name, got, want)
	}
	return nil
}

func equalValue(name string, got, want any) error {
	if fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Errorf("%s is %v, want %v", name, got, want)
	}
	return nil
}
