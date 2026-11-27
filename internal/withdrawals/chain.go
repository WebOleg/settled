package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// vaultABI is the part of IVault the payer calls, with the errors it tells apart. The
// end-to-end check (cmd/bridge-e2e) runs it against the vault the repository builds.
const vaultABI = `[
 {"type":"function","name":"withdraw","stateMutability":"nonpayable","inputs":[{"name":"id","type":"bytes32"},{"name":"token","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"status","type":"uint8"}]},
 {"type":"function","name":"executeQueued","stateMutability":"nonpayable","inputs":[{"name":"id","type":"bytes32"}],"outputs":[]},
 {"type":"function","name":"withdrawal","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"","type":"tuple","components":[{"name":"status","type":"uint8"},{"name":"releaseAt","type":"uint64"},{"name":"token","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]}]},
 {"type":"error","name":"WithdrawalExists","inputs":[{"name":"id","type":"bytes32"},{"name":"status","type":"uint8"}]},
 {"type":"error","name":"WithdrawalNotQueued","inputs":[{"name":"id","type":"bytes32"},{"name":"status","type":"uint8"}]},
 {"type":"error","name":"WithdrawalNotReleased","inputs":[{"name":"id","type":"bytes32"},{"name":"releaseAt","type":"uint64"}]},
 {"type":"error","name":"RateLimited","inputs":[{"name":"token","type":"address"},{"name":"requested","type":"uint256"},{"name":"available","type":"uint256"}]},
 {"type":"error","name":"InsufficientFunds","inputs":[{"name":"token","type":"address"},{"name":"requested","type":"uint256"},{"name":"accounted","type":"uint256"}]},
 {"type":"error","name":"EnforcedPause","inputs":[]},
 {"type":"error","name":"NativeTransferFailed","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}]},
 {"type":"error","name":"TokenNotAllowed","inputs":[{"name":"token","type":"address"}]},
 {"type":"error","name":"ZeroAddress","inputs":[]},
 {"type":"error","name":"ZeroAmount","inputs":[]},
 {"type":"error","name":"AccessControlUnauthorizedAccount","inputs":[{"name":"account","type":"address"},{"name":"neededRole","type":"bytes32"}]}
]`

// VaultABI is the parsed vaultABI.
var VaultABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(vaultABI))
	if err != nil {
		panic(err)
	}
	return a
}()

// Status is a withdrawal id's status in the vault (IVault.WithdrawalStatus).
type Status uint8

const (
	None Status = iota
	Queued
	Paid
	Cancelled
)

// OnChain is what the vault records for an id.
type OnChain struct {
	Status    Status
	ReleaseAt uint64
}

// Receipt is the part of a receipt the payer reads.
type Receipt struct {
	OK    bool
	Block uint64
}

// Chain is what the payer reads from and sends to a node.
type Chain interface {
	ChainID(ctx context.Context) (*big.Int, error)
	Head(ctx context.Context) (uint64, error)
	// Withdrawal reads the vault's record of id at the head.
	Withdrawal(ctx context.Context, vault common.Address, id common.Hash) (OnChain, error)
	// Estimate runs data from `from` to `to` without sending it: the gas it takes, or a
	// *Revert with the vault's error.
	Estimate(ctx context.Context, from, to common.Address, data []byte) (uint64, error)
	// Nonce is the account's nonce at the head: transactions mined.
	Nonce(ctx context.Context, account common.Address) (uint64, error)
	// Finalized is the node's finalized block, and the account's nonce there: what
	// no reorganisation can undo.
	Finalized(ctx context.Context, account common.Address) (block, nonce uint64, err error)
	// Fees are the tip and fee cap to sign with.
	Fees(ctx context.Context) (tip, cap *big.Int, err error)
	Send(ctx context.Context, tx *types.Transaction) error
	// Receipt returns nil when the transaction is not mined.
	Receipt(ctx context.Context, hash common.Hash) (*Receipt, error)
}

// Revert is a call the vault refused, with its custom error.
type Revert struct {
	Name string // the error's name in vaultABI, or "" when unknown
	Data []byte
}

func (r *Revert) Error() string {
	if r.Name != "" {
		return "the vault refuses: " + r.Name
	}
	return fmt.Sprintf("the vault refuses: %x", r.Data)
}

// decodeRevert names the custom error in revert data.
func decodeRevert(data []byte) *Revert {
	r := &Revert{Data: data}
	if len(data) >= 4 {
		for name, e := range VaultABI.Errors {
			if string(e.ID[:4]) == string(data[:4]) {
				r.Name = name
			}
		}
	}
	return r
}

// RPC is Chain over a node's JSON-RPC.
type RPC struct {
	Client *ethclient.Client
}

func (r RPC) ChainID(ctx context.Context) (*big.Int, error) { return r.Client.ChainID(ctx) }

func (r RPC) Head(ctx context.Context) (uint64, error) { return r.Client.BlockNumber(ctx) }

func (r RPC) Withdrawal(ctx context.Context, vault common.Address, id common.Hash) (OnChain, error) {
	data, err := VaultABI.Pack("withdrawal", id)
	if err != nil {
		return OnChain{}, err
	}
	out, err := r.Client.CallContract(ctx, ethereum.CallMsg{To: &vault, Data: data}, nil)
	if err != nil {
		return OnChain{}, fmt.Errorf("withdrawals: withdrawal(%s): %w", id, err)
	}
	vals, err := VaultABI.Unpack("withdrawal", out)
	if err != nil || len(vals) != 1 {
		return OnChain{}, fmt.Errorf("withdrawals: decode withdrawal(%s): %w", id, err)
	}
	type record struct {
		Status    uint8
		ReleaseAt uint64
		Token     common.Address
		To        common.Address
		Amount    *big.Int
	}
	w := *abi.ConvertType(vals[0], new(record)).(*record)
	return OnChain{Status: Status(w.Status), ReleaseAt: w.ReleaseAt}, nil
}

func (r RPC) Estimate(ctx context.Context, from, to common.Address, data []byte) (uint64, error) {
	gas, err := r.Client.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Data: data})
	if err == nil {
		return gas, nil
	}
	var de rpc.DataError
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok {
			if b, derr := hexutil.Decode(s); derr == nil && len(b) > 0 {
				return 0, decodeRevert(b)
			}
		}
	}
	return 0, fmt.Errorf("withdrawals: estimate gas: %w", err)
}

func (r RPC) Nonce(ctx context.Context, account common.Address) (uint64, error) {
	return r.Client.NonceAt(ctx, account, nil)
}

func (r RPC) Finalized(ctx context.Context, account common.Address) (uint64, uint64, error) {
	final := big.NewInt(int64(rpc.FinalizedBlockNumber))
	h, err := r.Client.HeaderByNumber(ctx, final)
	if err != nil {
		return 0, 0, fmt.Errorf("withdrawals: finalized block: %w", err)
	}
	n, err := r.Client.NonceAt(ctx, account, h.Number)
	if err != nil {
		return 0, 0, fmt.Errorf("withdrawals: nonce at the finalized block: %w", err)
	}
	return h.Number.Uint64(), n, nil
}

func (r RPC) Fees(ctx context.Context) (*big.Int, *big.Int, error) {
	tip, err := r.Client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("withdrawals: gas tip: %w", err)
	}
	head, err := r.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("withdrawals: head: %w", err)
	}
	if head.BaseFee == nil {
		return nil, nil, errors.New("withdrawals: the head has no base fee")
	}
	return tip, new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip), nil
}

func (r RPC) Send(ctx context.Context, tx *types.Transaction) error {
	return r.Client.SendTransaction(ctx, tx)
}

func (r RPC) Receipt(ctx context.Context, hash common.Hash) (*Receipt, error) {
	rc, err := r.Client.TransactionReceipt(ctx, hash)
	if errors.Is(err, ethereum.NotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("withdrawals: receipt %s: %w", hash, err)
	}
	return &Receipt{OK: rc.Status == types.ReceiptStatusSuccessful, Block: rc.BlockNumber.Uint64()}, nil
}
