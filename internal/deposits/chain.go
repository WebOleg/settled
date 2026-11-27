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

// TopicDeposited is keccak256 of the vault's Deposited event (IVault).
var TopicDeposited = crypto.Keccak256Hash([]byte("Deposited(uint256,address,address,bytes32,uint256)"))

// Native is the address that stands for ETH in the vault (ERC-7528).
var Native = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")

// Header is the part of a block header the indexer reads.
type Header struct {
	Number uint64
	Hash   common.Hash
	Parent common.Hash
}

// Event is one Deposited event of the vault.
type Event struct {
	Block     uint64
	BlockHash common.Hash
	TxHash    common.Hash
	LogIndex  uint
	DepositID *big.Int
	Token     common.Address
	Sender    common.Address
	Account   common.Hash
	Amount    *big.Int
}

// Chain is what the indexer reads from a node.
type Chain interface {
	Latest(ctx context.Context) (Header, error)
	Finalized(ctx context.Context) (Header, error)
	Header(ctx context.Context, n uint64) (Header, error)
	// EventsInBlock returns the vault's deposits in the block with this hash, so they
	// are the ones of that block even if the chain moves while they are read.
	EventsInBlock(ctx context.Context, vault common.Address, block common.Hash) ([]Event, error)
	// EventsInRange returns the vault's deposits in blocks from..to, all final.
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

// EventsInBlock reads the deposits of one block by its hash (EIP-234).
func (r RPC) EventsInBlock(ctx context.Context, vault common.Address, block common.Hash) ([]Event, error) {
	return r.events(ctx, ethereum.FilterQuery{BlockHash: &block, Addresses: []common.Address{vault}, Topics: [][]common.Hash{{TopicDeposited}}})
}

// EventsInRange reads the deposits of blocks from..to.
func (r RPC) EventsInRange(ctx context.Context, vault common.Address, from, to uint64) ([]Event, error) {
	return r.events(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to),
		Addresses: []common.Address{vault}, Topics: [][]common.Hash{{TopicDeposited}},
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

// Decode reads a Deposited log: the id, token and sender are indexed, the account and
// amount are the data.
func Decode(l types.Log) (Event, error) {
	if l.Removed {
		return Event{}, fmt.Errorf("deposits: log %s/%d was removed by a reorganisation", l.BlockHash, l.Index)
	}
	if len(l.Topics) != 4 || l.Topics[0] != TopicDeposited {
		return Event{}, fmt.Errorf("deposits: log %s/%d is not Deposited", l.BlockHash, l.Index)
	}
	if len(l.Data) != 64 {
		return Event{}, fmt.Errorf("deposits: Deposited %s/%d has %d bytes of data, want 64", l.BlockHash, l.Index, len(l.Data))
	}
	amount := new(big.Int).SetBytes(l.Data[32:])
	if amount.Sign() == 0 {
		return Event{}, errors.New("deposits: Deposited with amount 0")
	}
	return Event{
		Block:     l.BlockNumber,
		BlockHash: l.BlockHash,
		TxHash:    l.TxHash,
		LogIndex:  l.Index,
		DepositID: new(big.Int).SetBytes(l.Topics[1][:]),
		Token:     common.BytesToAddress(l.Topics[2][:]),
		Sender:    common.BytesToAddress(l.Topics[3][:]),
		Account:   common.BytesToHash(l.Data[:32]),
		Amount:    amount,
	}, nil
}
