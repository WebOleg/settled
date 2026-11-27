package deposits

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Topics of the vault's events the indexer reads (IVault), keccak256 of the signatures.
var (
	TopicDeposited = crypto.Keccak256Hash([]byte("Deposited(uint256,address,address,bytes32,uint256)"))
	TopicPaid      = crypto.Keccak256Hash([]byte("WithdrawalPaid(bytes32,address,address,uint256)"))
	TopicCancelled = crypto.Keccak256Hash([]byte("WithdrawalCancelled(bytes32,address)"))
	topics         = [][]common.Hash{{TopicDeposited, TopicPaid, TopicCancelled}}
)

// Kind is which of the vault's events an Event is.
type Kind string

const (
	KindDeposit   Kind = "deposit"
	KindPaid      Kind = "paid"
	KindCancelled Kind = "cancelled"
)

// Native is the address that stands for ETH in the vault (ERC-7528).
var Native = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")

// Header is the part of a block header the indexer reads.
type Header struct {
	Number uint64
	Hash   common.Hash
	Parent common.Hash
}

// Event is one of the vault's events that moves money: a deposit, a payment, or a
// cancellation of a queued withdrawal.
type Event struct {
	Kind      Kind
	Block     uint64
	BlockHash common.Hash
	TxHash    common.Hash
	LogIndex  uint
	// A deposit: its id, the depositor and the exchange account it is for.
	DepositID *big.Int
	Sender    common.Address
	Account   common.Hash
	// A payment or a cancellation: the withdrawal id; for a payment, its recipient.
	WithdrawalID common.Hash
	Recipient    common.Address
	// Token and Amount, for a deposit and a payment.
	Token  common.Address
	Amount *big.Int
}

// Chain is what the indexer reads from a node.
type Chain interface {
	Latest(ctx context.Context) (Header, error)
	Finalized(ctx context.Context) (Header, error)
	Header(ctx context.Context, n uint64) (Header, error)
	// EventsInBlock returns the vault's events in the block with this hash, so they
	// are the ones of that block even if the chain moves while they are read.
	EventsInBlock(ctx context.Context, vault common.Address, block common.Hash) ([]Event, error)
	// EventsInRange returns the vault's events in blocks from..to, all final.
	EventsInRange(ctx context.Context, vault common.Address, from, to uint64) ([]Event, error)
}

// RPC reads the chain from a node's JSON-RPC.
type RPC struct {
	Client *ethclient.Client
}

func (r RPC) header(ctx context.Context, n *big.Int) (Header, error) {
	h, err := r.Client.HeaderByNumber(ctx, n)
	if err != nil {
		return Header{}, fmt.Errorf("deposits: header %v: %w", n, err)
	}
	return Header{Number: h.Number.Uint64(), Hash: h.Hash(), Parent: h.ParentHash}, nil
}

// Latest is the node's head: blocks the sequencer made, not yet final.
func (r RPC) Latest(ctx context.Context) (Header, error) { return r.header(ctx, nil) }

// Finalized is the latest block the node holds final: derived from L1 data that the
// L1 has finalized.
func (r RPC) Finalized(ctx context.Context) (Header, error) {
	return r.header(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
}

// Header is block n of the node's chain.
func (r RPC) Header(ctx context.Context, n uint64) (Header, error) {
	return r.header(ctx, new(big.Int).SetUint64(n))
}

// EventsInBlock reads the events of one block by its hash (EIP-234).
func (r RPC) EventsInBlock(ctx context.Context, vault common.Address, block common.Hash) ([]Event, error) {
	return r.events(ctx, ethereum.FilterQuery{BlockHash: &block, Addresses: []common.Address{vault}, Topics: topics})
}

// EventsInRange reads the events of blocks from..to.
func (r RPC) EventsInRange(ctx context.Context, vault common.Address, from, to uint64) ([]Event, error) {
	return r.events(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to),
		Addresses: []common.Address{vault}, Topics: topics,
	})
}

func (r RPC) events(ctx context.Context, q ethereum.FilterQuery) ([]Event, error) {
	logs, err := r.Client.FilterLogs(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("deposits: read logs: %w", err)
	}
	out := make([]Event, 0, len(logs))
	for _, l := range logs {
		e, err := Decode(l)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Decode reads one of the vault's logs. Deposited: the id, token and sender are
// indexed, the account and amount are the data. WithdrawalPaid: the id, token and
// recipient are indexed, the amount is the data. WithdrawalCancelled: the id and the
// guardian are indexed.
func Decode(l types.Log) (Event, error) {
	if l.Removed {
		return Event{}, fmt.Errorf("deposits: log %s/%d was removed by a reorganisation", l.BlockHash, l.Index)
	}
	e := Event{Block: l.BlockNumber, BlockHash: l.BlockHash, TxHash: l.TxHash, LogIndex: l.Index}
	bad := func(what string) (Event, error) {
		return Event{}, fmt.Errorf("deposits: log %s/%d: %s", l.BlockHash, l.Index, what)
	}
	if len(l.Topics) == 0 {
		return bad("no topics")
	}
	switch l.Topics[0] {
	case TopicDeposited:
		if len(l.Topics) != 4 || len(l.Data) != 64 {
			return bad("not a Deposited event")
		}
		e.Kind = KindDeposit
		e.DepositID = new(big.Int).SetBytes(l.Topics[1][:])
		e.Token = common.BytesToAddress(l.Topics[2][:])
		e.Sender = common.BytesToAddress(l.Topics[3][:])
		e.Account = common.BytesToHash(l.Data[:32])
		e.Amount = new(big.Int).SetBytes(l.Data[32:])
	case TopicPaid:
		if len(l.Topics) != 4 || len(l.Data) != 32 {
			return bad("not a WithdrawalPaid event")
		}
		e.Kind = KindPaid
		e.WithdrawalID = l.Topics[1]
		e.Token = common.BytesToAddress(l.Topics[2][:])
		e.Recipient = common.BytesToAddress(l.Topics[3][:])
		e.Amount = new(big.Int).SetBytes(l.Data)
	case TopicCancelled:
		if len(l.Topics) != 3 || len(l.Data) != 0 {
			return bad("not a WithdrawalCancelled event")
		}
		e.Kind = KindCancelled
		e.WithdrawalID = l.Topics[1]
		return e, nil
	default:
		return bad("not an event the indexer reads")
	}
	if e.Amount.Sign() == 0 {
		return Event{}, errors.New("deposits: an event with amount 0")
	}
	return e, nil
}
