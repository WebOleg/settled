// Command l2-withdraw moves ETH from the L2 back to the L1, the whole way, and accounts
// for every wei. The devnet user (index 2 of the public devnet mnemonic, read from
// SETTLED_DEVNET_MNEMONIC and never printed) withdraws to its own address:
//
//  1. Start: L2ToL1MessagePasser.initiateWithdrawal on the L2, sent to the verifier,
//     which forwards it to the sequencer. The withdrawal hash computed here must equal
//     the one the contract emits, and the user pays exactly the amount, the execution
//     gas and the L1 data fee.
//  2. Wait for a dispute game whose L2 block is at or after the withdrawal, and check its
//     root claim against a super root recomputed from the verifier before using it.
//  3. Prove on the L1, with the output root opened and a storage proof that the
//     withdrawal is recorded in the L2ToL1MessagePasser at the game's block.
//  4. Wait until the portal would accept finalization (proof maturity delay, game
//     finality delay), then finalize. The lockbox pays out exactly the amount, the user
//     receives it less the gas, and the portal marks the withdrawal finalized.
//  5. A second finalization of the same withdrawal must be refused.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/disputegame"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/outputroot"
	"github.com/WebOleg/settled/internal/portal"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/txsend"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// UserIndex is the devnet user, the same account l2-deposit funds on the L2.
	UserIndex = 2
	// WithdrawalGasLimit is the gas the L1 call to the target gets when the withdrawal
	// is finalized; the target is an account with no code.
	WithdrawalGasLimit = 100_000
	// GameType is the respected game type: SUPER_PERMISSIONED_CANNON.
	GameType = 5
)

// messagePassedTopic is keccak256("MessagePassed(uint256,address,address,uint256,uint256,bytes,bytes32)").
var messagePassedTopic = common.HexToHash("0x02a52367d10742d8032712c1bb8e0144ff1ec5ffda1ed7d70bb05a2744955054")

type env struct {
	l1       *ethclient.Client
	l2       *ethclient.Client
	l2raw    *rpc.Client
	key      *ecdsa.PrivateKey
	user     common.Address
	portal   common.Address
	lockbox  common.Address
	factory  common.Address
	rollup   disputegame.Rollup
	proposer common.Address
}

func main() {
	l1URL := flag.String("l1", "http://127.0.0.1:8545", "L1 execution JSON-RPC")
	l2URL := flag.String("l2", "http://127.0.0.1:28545", "verifier execution JSON-RPC on the L2")
	dir := flag.String("deployer-dir", ".devnet/l2/deployer", "files written by make l2-deploy")
	rolesFile := flag.String("roles", ".devnet/l2/keys/"+rolekeys.AddressesFile, "role addresses written by make l2-keys")
	milli := flag.Int64("milliether", 100, "amount to withdraw, in thousandths of an ETH")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	amount := new(big.Int).Mul(big.NewInt(*milli), big.NewInt(1e15))
	if err := run(ctx, *l1URL, *l2URL, *dir, *rolesFile, amount); err != nil {
		fmt.Fprintf(os.Stderr, "l2-withdraw: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("l2-withdraw: ok")
}

func run(ctx context.Context, l1URL, l2URL, dir, rolesFile string, amount *big.Int) error {
	if amount.Sign() <= 0 {
		return errors.New("amount must be positive")
	}
	e, err := setup(ctx, l1URL, l2URL, dir, rolesFile)
	if err != nil {
		return err
	}
	defer e.l1.Close()
	defer e.l2raw.Close()
	fmt.Printf("user             %s (devnet mnemonic index %d)\n", e.user, UserIndex)
	fmt.Printf("amount           %s ETH (%s wei)\n", funding.FormatEther(amount), amount)

	wtx, wBlock, err := start(ctx, e, amount)
	if err != nil {
		return err
	}
	hash, err := portal.Hash(wtx)
	if err != nil {
		return err
	}
	game, out, err := waitForGame(ctx, e, wBlock, portal.SentMessageSlot(hash))
	if err != nil {
		return err
	}
	proveReceipt, err := prove(ctx, e, wtx, game, out)
	if err != nil {
		return err
	}
	finalizeReceipt, err := finalize(ctx, e, wtx, amount)
	if err != nil {
		return err
	}

	// The whole way on the L1, from the block before the proof to the block of the
	// finalization: the user gained the amount and paid the gas of both transactions.
	before, err := e.l1.BalanceAt(ctx, e.user, new(big.Int).Sub(proveReceipt.BlockNumber, big.NewInt(1)))
	if err != nil {
		return err
	}
	after, err := e.l1.BalanceAt(ctx, e.user, finalizeReceipt.BlockNumber)
	if err != nil {
		return err
	}
	gas := new(big.Int).Add(txsend.ExecutionCost(proveReceipt), txsend.ExecutionCost(finalizeReceipt))
	want := new(big.Int).Sub(amount, gas)
	if got := new(big.Int).Sub(after, before); got.Cmp(want) != 0 {
		return fmt.Errorf("over prove and finalize the user's L1 balance changed by %s wei, want amount - gas = %s", got, want)
	}
	fmt.Printf("L1 user overall  +%s wei = amount - gas of prove and finalize (%s wei)\n", want, gas)

	return refuseReplay(ctx, e, wtx)
}

func setup(ctx context.Context, l1URL, l2URL, dir, rolesFile string) (*env, error) {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return nil, fmt.Errorf("%s is not set", mnemonicEnv)
	}
	key, err := devkeys.FromMnemonic(mnemonic, UserIndex)
	if err != nil {
		return nil, fmt.Errorf("derive the devnet user key: %w", err)
	}
	e := &env{key: key, user: crypto.PubkeyToAddress(key.PublicKey)}
	contracts := map[string]common.Address{}
	if err := readJSON(filepath.Join(dir, "l1-addresses.json"), &contracts); err != nil {
		return nil, err
	}
	e.portal, e.lockbox, e.factory = contracts["OptimismPortalProxy"], contracts["EthLockboxProxy"], contracts["DisputeGameFactoryProxy"]
	if e.portal == (common.Address{}) || e.lockbox == (common.Address{}) || e.factory == (common.Address{}) {
		return nil, errors.New("l1-addresses.json lacks the portal, the lockbox or the factory")
	}
	if err := readJSON(filepath.Join(dir, "rollup.json"), &e.rollup); err != nil {
		return nil, err
	}
	var entries []rolekeys.Entry
	if err := readJSON(rolesFile, &entries); err != nil {
		return nil, err
	}
	for _, r := range entries {
		if r.Role == rolekeys.Proposer {
			e.proposer = r.Address
		}
	}
	if e.l1, err = ethclient.DialContext(ctx, l1URL); err != nil {
		return nil, fmt.Errorf("connect to the L1: %w", err)
	}
	if e.l2raw, err = rpc.DialContext(ctx, l2URL); err != nil {
		return nil, fmt.Errorf("connect to the L2: %w", err)
	}
	e.l2 = ethclient.NewClient(e.l2raw)
	return e, nil
}

// start initiates the withdrawal on the L2 and checks the event and the L2 accounting.
func start(ctx context.Context, e *env, amount *big.Int) (portal.WithdrawalTx, uint64, error) {
	var wtx portal.WithdrawalTx
	gasLimit := big.NewInt(WithdrawalGasLimit)
	data, err := portal.InitiateWithdrawal(e.user, gasLimit, nil)
	if err != nil {
		return wtx, 0, err
	}
	receipt, err := txsend.Send(ctx, e.l2, e.key, outputroot.MessagePasser, amount, data)
	if err != nil {
		return wtx, 0, fmt.Errorf("initiate the withdrawal: %w", err)
	}
	block := receipt.BlockNumber.Uint64()
	fmt.Printf("L2 start         tx %s in L2 block %d\n", receipt.TxHash, block)

	var event *types.Log
	for _, l := range receipt.Logs {
		if l.Address == outputroot.MessagePasser && len(l.Topics) == 4 && l.Topics[0] == messagePassedTopic {
			event = l
		}
	}
	if event == nil || len(event.Data) < 4*32 {
		return wtx, 0, errors.New("no MessagePassed event in the receipt")
	}
	word := func(i int) []byte { return event.Data[i*32 : (i+1)*32] }
	wtx = portal.WithdrawalTx{
		Nonce:    new(big.Int).SetBytes(event.Topics[1][:]),
		Sender:   common.BytesToAddress(event.Topics[2][:]),
		Target:   common.BytesToAddress(event.Topics[3][:]),
		Value:    new(big.Int).SetBytes(word(0)),
		GasLimit: new(big.Int).SetBytes(word(1)),
		Data:     []byte{},
	}
	emitted := common.BytesToHash(word(3))
	if wtx.Sender != e.user || wtx.Target != e.user || wtx.Value.Cmp(amount) != 0 || wtx.GasLimit.Cmp(gasLimit) != 0 {
		return wtx, 0, fmt.Errorf("the event does not describe this withdrawal: %+v", wtx)
	}
	hash, err := portal.Hash(wtx)
	if err != nil {
		return wtx, 0, err
	}
	if hash != emitted {
		return wtx, 0, fmt.Errorf("withdrawal hash computed %s, emitted %s", hash, emitted)
	}
	fmt.Printf("withdrawal       nonce %s, hash %s (computed here = emitted by the contract)\n", wtx.Nonce, hash)

	// L2 accounting at the block before and the block of the withdrawal.
	var raw struct {
		L1Fee *hexutil.Big `json:"l1Fee"`
	}
	if err := e.l2raw.CallContext(ctx, &raw, "eth_getTransactionReceipt", receipt.TxHash); err != nil {
		return wtx, 0, fmt.Errorf("read the L2 receipt: %w", err)
	}
	if raw.L1Fee == nil {
		return wtx, 0, errors.New("the L2 receipt has no l1Fee")
	}
	prev, at := new(big.Int).SetUint64(block-1), new(big.Int).SetUint64(block)
	before, err := e.l2.BalanceAt(ctx, e.user, prev)
	if err != nil {
		return wtx, 0, err
	}
	after, err := e.l2.BalanceAt(ctx, e.user, at)
	if err != nil {
		return wtx, 0, err
	}
	exec := txsend.ExecutionCost(receipt)
	want := new(big.Int).Add(new(big.Int).Add(amount, exec), raw.L1Fee.ToInt())
	if paid := new(big.Int).Sub(before, after); paid.Cmp(want) != 0 {
		return wtx, 0, fmt.Errorf("the user paid %s wei on the L2, want amount + execution + L1 fee = %s", paid, want)
	}
	fmt.Printf("L2 user          paid amount + execution + L1 data fee: %s + %s + %s wei\n", amount, exec, raw.L1Fee.ToInt())
	passerBefore, err := e.l2.BalanceAt(ctx, outputroot.MessagePasser, prev)
	if err != nil {
		return wtx, 0, err
	}
	passerAfter, err := e.l2.BalanceAt(ctx, outputroot.MessagePasser, at)
	if err != nil {
		return wtx, 0, err
	}
	if got := new(big.Int).Sub(passerAfter, passerBefore); got.Cmp(amount) != 0 {
		return wtx, 0, fmt.Errorf("L2ToL1MessagePasser grew by %s wei, want %s", got, amount)
	}
	fmt.Printf("L2 passer        +%s wei held until the withdrawal is finalized on the L1\n", amount)
	return wtx, block, nil
}

// waitForGame waits for a valid game at or after the withdrawal's L2 block and returns it
// with the output at its block, including the proof of the withdrawal's slot.
func waitForGame(ctx context.Context, e *env, wBlock uint64, slot common.Hash) (disputegame.Game, disputegame.Output, error) {
	started := time.Now()
	fmt.Printf("waiting          for a dispute game at or after L2 block %d (proposals cover finalized blocks only)\n", wBlock)
	for {
		count, err := disputegame.Count(ctx, e.l1, e.factory)
		if err != nil {
			return disputegame.Game{}, disputegame.Output{}, err
		}
		if count > 0 {
			g, err := disputegame.Read(ctx, e.l1, e.factory, count-1)
			if err != nil {
				return g, disputegame.Output{}, err
			}
			number, err := e.rollup.BlockAt(g.Timestamp)
			if err != nil {
				return g, disputegame.Output{}, err
			}
			if number >= wBlock {
				return checkGame(ctx, e, g, number, slot, time.Since(started))
			}
		}
		select {
		case <-ctx.Done():
			return disputegame.Game{}, disputegame.Output{}, fmt.Errorf("no game covers L2 block %d in time: %w", wBlock, ctx.Err())
		case <-time.After(15 * time.Second):
		}
	}
}

func checkGame(ctx context.Context, e *env, g disputegame.Game, number uint64, slot common.Hash, waited time.Duration) (disputegame.Game, disputegame.Output, error) {
	if g.Type != GameType || g.Creator != e.proposer || g.Status != disputegame.DefenderWins || !g.Respected {
		return g, disputegame.Output{}, fmt.Errorf("game %d is not a valid proposal: %+v", g.Index, g)
	}
	out, err := disputegame.OutputAt(ctx, e.l2raw, number, slot)
	if err != nil {
		return g, out, err
	}
	if want := e.rollup.SuperRoot(g.Timestamp, out); g.RootClaim != want {
		return g, out, fmt.Errorf("game %d claims %s, recomputed %s; not proving against it", g.Index, g.RootClaim, want)
	}
	if v := out.Slots[0].Value; v == nil || v.ToInt().Cmp(big.NewInt(1)) != 0 {
		return g, out, fmt.Errorf("the withdrawal's slot is not set at L2 block %d", number)
	}
	fmt.Printf("game             %d at L2 block %d after %s; root claim recomputed from the verifier; slot set\n",
		g.Index, number, waited.Round(time.Second))
	return g, out, nil
}

func prove(ctx context.Context, e *env, wtx portal.WithdrawalTx, g disputegame.Game, out disputegame.Output) (*types.Receipt, error) {
	proofNodes := make([][]byte, len(out.Slots[0].Proof))
	for i, p := range out.Slots[0].Proof {
		proofNodes[i] = p
	}
	data, err := portal.Prove(wtx, new(big.Int).SetUint64(g.Index), portal.OutputRootProof{
		StateRoot:                out.StateRoot,
		MessagePasserStorageRoot: out.PasserRoot,
		LatestBlockhash:          out.BlockHash,
	}, proofNodes)
	if err != nil {
		return nil, err
	}
	receipt, err := txsend.Send(ctx, e.l1, e.key, e.portal, new(big.Int), data)
	if err != nil {
		return nil, fmt.Errorf("prove: %w", err)
	}
	fmt.Printf("L1 prove         tx %s in L1 block %s, %d proof nodes\n", receipt.TxHash, receipt.BlockNumber, len(proofNodes))
	return receipt, nil
}

func finalize(ctx context.Context, e *env, wtx portal.WithdrawalTx, amount *big.Int) (*types.Receipt, error) {
	data, err := portal.Finalize(wtx)
	if err != nil {
		return nil, err
	}
	// Ready when the portal would accept the call: the proof has matured and the game's
	// finality delay has passed.
	started := time.Now()
	for {
		_, err := e.l1.EstimateGas(ctx, ethereum.CallMsg{From: e.user, To: &e.portal, Data: data})
		if err == nil {
			break
		}
		if time.Since(started) > 5*time.Minute {
			return nil, fmt.Errorf("the portal still refuses finalization after 5 minutes: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(6 * time.Second):
		}
	}
	fmt.Printf("ready            the portal accepts finalization after %s\n", time.Since(started).Round(time.Second))

	receipt, err := txsend.Send(ctx, e.l1, e.key, e.portal, new(big.Int), data)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	block := receipt.BlockNumber
	prev := new(big.Int).Sub(block, big.NewInt(1))
	fmt.Printf("L1 finalize      tx %s in L1 block %s\n", receipt.TxHash, block)

	custody := new(big.Int)
	for _, addr := range []common.Address{e.lockbox, e.portal} {
		b, err := e.l1.BalanceAt(ctx, addr, prev)
		if err != nil {
			return nil, err
		}
		a, err := e.l1.BalanceAt(ctx, addr, block)
		if err != nil {
			return nil, err
		}
		custody.Add(custody, new(big.Int).Sub(b, a))
	}
	if custody.Cmp(amount) != 0 {
		return nil, fmt.Errorf("the lockbox and the portal paid out %s wei, want %s", custody, amount)
	}
	before, err := e.l1.BalanceAt(ctx, e.user, prev)
	if err != nil {
		return nil, err
	}
	after, err := e.l1.BalanceAt(ctx, e.user, block)
	if err != nil {
		return nil, err
	}
	gas := txsend.ExecutionCost(receipt)
	if got, want := new(big.Int).Sub(after, before), new(big.Int).Sub(amount, gas); got.Cmp(want) != 0 {
		return nil, fmt.Errorf("the user received %s wei, want amount - gas = %s", got, want)
	}
	fmt.Printf("L1 payout        custody -%s wei, user +%s wei = amount - gas %s\n", amount, new(big.Int).Sub(amount, gas), gas)

	hash, err := portal.Hash(wtx)
	if err != nil {
		return nil, err
	}
	call, err := portal.FinalizedWithdrawals(hash)
	if err != nil {
		return nil, err
	}
	res, err := e.l1.CallContract(ctx, ethereum.CallMsg{To: &e.portal, Data: call}, block)
	if err != nil {
		return nil, err
	}
	if len(res) != 32 || chaincall.UintOf(common.BytesToHash(res)).Uint64() != 1 {
		return nil, errors.New("the portal does not mark the withdrawal finalized")
	}
	fmt.Println("L1 state         finalizedWithdrawals(hash) = true")
	return receipt, nil
}

func refuseReplay(ctx context.Context, e *env, wtx portal.WithdrawalTx) error {
	data, err := portal.Finalize(wtx)
	if err != nil {
		return err
	}
	_, err = e.l1.EstimateGas(ctx, ethereum.CallMsg{From: e.user, To: &e.portal, Data: data})
	if err == nil {
		return errors.New("a second finalization of the same withdrawal would be accepted")
	}
	fmt.Printf("replay           a second finalization is refused: %v\n", err)
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
