// Package timelockop deploys an OpenZeppelin TimelockController with its roles split,
// and runs one call through it the way every admin change of the vault goes: schedule
// it, wait until the timelock says it is ready, execute it. The id of the operation is
// read from the timelock itself, so it is the one the timelock checks.
package timelockop

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/txsend"
)

// The roles of a TimelockController.
var (
	AdminRole     = common.Hash{}
	ProposerRole  = crypto.Keccak256Hash([]byte("PROPOSER_ROLE"))
	ExecutorRole  = crypto.Keccak256Hash([]byte("EXECUTOR_ROLE"))
	CancellerRole = crypto.Keccak256Hash([]byte("CANCELLER_ROLE"))
)

// Deploy creates a TimelockController with delay whose roles are split: proposer
// proposes and executes, canceller only cancels, and the timelock alone administers
// itself, so its roles change only through its own delay. The constructor makes every
// proposer a canceller too; to take that back the deployer is admin for three calls,
// grant, revoke and renounce, and holds nothing after them. A stolen proposer key can
// then propose, but the canceller stops it within the delay.
func Deploy(ctx context.Context, client *ethclient.Client, deployer *ecdsa.PrivateKey, timelock *artifact.Artifact,
	delay *big.Int, proposer, canceller common.Address,
) (common.Address, *types.Receipt, error) {
	if proposer == canceller {
		return common.Address{}, nil, errors.New("timelockop: the proposer and the canceller must be different keys")
	}
	self := crypto.PubkeyToAddress(deployer.PublicKey)
	data, err := timelock.DeployData(delay, []common.Address{proposer}, []common.Address{proposer}, self)
	if err != nil {
		return common.Address{}, nil, err
	}
	receipt, at, err := txsend.Deploy(ctx, client, deployer, data)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("timelockop: deploy: %w", err)
	}
	for _, step := range []struct {
		method  string
		role    common.Hash
		account common.Address
	}{
		{"grantRole", CancellerRole, canceller},
		{"revokeRole", CancellerRole, proposer},
		{"renounceRole", AdminRole, self},
	} {
		call, err := timelock.Call(step.method, [32]byte(step.role), step.account)
		if err != nil {
			return at, receipt, err
		}
		if _, err := txsend.Send(ctx, client, deployer, at, new(big.Int), call); err != nil {
			return at, receipt, fmt.Errorf("timelockop: %s: %w", step.method, err)
		}
	}
	for _, want := range []struct {
		role    common.Hash
		account common.Address
		holds   bool
	}{
		{ProposerRole, proposer, true}, {ExecutorRole, proposer, true}, {CancellerRole, proposer, false},
		{CancellerRole, canceller, true}, {ProposerRole, canceller, false}, {ExecutorRole, canceller, false},
		{AdminRole, at, true}, {AdminRole, self, false},
	} {
		holds, err := HasRole(ctx, client, timelock, at, want.role, want.account)
		if err != nil {
			return at, receipt, err
		}
		if holds != want.holds {
			return at, receipt, fmt.Errorf("timelockop: %s holds role %s: %v, want %v", want.account, want.role, holds, want.holds)
		}
	}
	return at, receipt, nil
}

// HasRole reads hasRole(role, account) of the timelock at `at`.
func HasRole(ctx context.Context, client *ethclient.Client, timelock *artifact.Artifact, at common.Address, role common.Hash, account common.Address) (bool, error) {
	w, err := word(ctx, client, timelock, at, "hasRole", [32]byte(role), account)
	if err != nil {
		return false, err
	}
	return w == common.BigToHash(big.NewInt(1)), nil
}

// Op is one call the timelock makes: data to target, with value, after predecessor.
type Op struct {
	Target      common.Address
	Value       *big.Int
	Data        []byte
	Predecessor [32]byte
	Salt        [32]byte
}

// PollInterval is how often Run asks whether the operation is ready.
const PollInterval = 2 * time.Second

// Run schedules op with delay from key, waits until it is ready, executes it, and
// returns the execution receipt and the operation id. Before each step, progress is
// passed to report.
func Run(ctx context.Context, client *ethclient.Client, key *ecdsa.PrivateKey, timelock *artifact.Artifact,
	at common.Address, op Op, delay *big.Int, report func(string),
) (*types.Receipt, common.Hash, error) {
	if op.Value == nil {
		op.Value = new(big.Int)
	}
	schedule, err := timelock.Call("schedule", op.Target, op.Value, op.Data, op.Predecessor, op.Salt, delay)
	if err != nil {
		return nil, common.Hash{}, err
	}
	if _, err := txsend.Send(ctx, client, key, at, new(big.Int), schedule); err != nil {
		return nil, common.Hash{}, fmt.Errorf("timelockop: schedule: %w", err)
	}
	id, err := word(ctx, client, timelock, at, "hashOperation", op.Target, op.Value, op.Data, op.Predecessor, op.Salt)
	if err != nil {
		return nil, common.Hash{}, err
	}
	report(fmt.Sprintf("scheduled operation %s; waiting %s s", id, delay))

	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		ready, err := word(ctx, client, timelock, at, "isOperationReady", id)
		if err != nil {
			return nil, id, err
		}
		if ready == common.BigToHash(big.NewInt(1)) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, id, fmt.Errorf("timelockop: operation %s not ready in time: %w", id, ctx.Err())
		case <-tick.C:
		}
	}
	execute, err := timelock.Call("execute", op.Target, op.Value, op.Data, op.Predecessor, op.Salt)
	if err != nil {
		return nil, id, err
	}
	receipt, err := txsend.Send(ctx, client, key, at, op.Value, execute)
	if err != nil {
		return nil, id, fmt.Errorf("timelockop: execute: %w", err)
	}
	done, err := word(ctx, client, timelock, at, "isOperationDone", id)
	if err != nil {
		return receipt, id, err
	}
	if done != common.BigToHash(big.NewInt(1)) {
		return receipt, id, fmt.Errorf("timelockop: operation %s executed but not done", id)
	}
	return receipt, id, nil
}

// word calls a view of the timelock that returns one 32-byte word.
func word(ctx context.Context, client *ethclient.Client, timelock *artifact.Artifact, at common.Address, method string, args ...any) (common.Hash, error) {
	data, err := timelock.Call(method, args...)
	if err != nil {
		return common.Hash{}, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &at, Data: data}, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("timelockop: %s: %w", method, err)
	}
	if len(out) != 32 {
		return common.Hash{}, fmt.Errorf("timelockop: %s: %w", method, errors.New("want one word"))
	}
	return common.BytesToHash(out), nil
}
