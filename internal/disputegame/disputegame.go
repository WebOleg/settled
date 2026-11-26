// Package disputegame reads the dispute games of a DisputeGameFactory and recomputes
// what a game claims from the L2 itself. l2-games uses it to check every proposal, and
// l2-withdraw to find the game a withdrawal is proven against and to build the proof.
package disputegame

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/outputroot"
)

// Status values of IDisputeGame.status().
const (
	InProgress     = 0
	ChallengerWins = 1
	DefenderWins   = 2
)

// StatusName returns the name of a game status.
func StatusName(s uint64) string {
	switch s {
	case InProgress:
		return "IN_PROGRESS"
	case ChallengerWins:
		return "CHALLENGER_WINS"
	case DefenderWins:
		return "DEFENDER_WINS"
	}
	return fmt.Sprintf("status %d", s)
}

// Game is one game as the factory and the game contract report it.
type Game struct {
	Index     uint64
	Type      uint64
	Proxy     common.Address
	RootClaim common.Hash
	Timestamp uint64 // the L2 timestamp the super root is for (l2SequenceNumber)
	Creator   common.Address
	Proposer  common.Address // the proposer the game accepts
	Status    uint64
	Respected bool // the game type was the respected one when the game was created
}

// Count returns the number of games the factory has created.
func Count(ctx context.Context, c chaincall.Caller, factory common.Address) (uint64, error) {
	w, err := chaincall.Word(ctx, c, factory, "gameCount()")
	if err != nil {
		return 0, err
	}
	return chaincall.UintOf(w).Uint64(), nil
}

// Read returns game index of the factory.
func Read(ctx context.Context, c chaincall.Caller, factory common.Address, index uint64) (Game, error) {
	g := Game{Index: index}
	ws, err := chaincall.Words(ctx, c, factory, "gameAtIndex(uint256)", chaincall.UintWord(index))
	if err != nil {
		return g, err
	}
	if len(ws) != 3 {
		return g, fmt.Errorf("disputegame: gameAtIndex returned %d words, want 3", len(ws))
	}
	g.Type = chaincall.UintOf(ws[0]).Uint64()
	if g.Proxy, err = chaincall.AddressOf(ws[2]); err != nil {
		return g, err
	}
	word := func(sig string) (common.Hash, error) { return chaincall.Word(ctx, c, g.Proxy, sig) }
	num := func(sig string) (uint64, error) {
		w, err := word(sig)
		return chaincall.UintOf(w).Uint64(), err
	}
	addr := func(sig string) (common.Address, error) {
		w, err := word(sig)
		if err != nil {
			return common.Address{}, err
		}
		return chaincall.AddressOf(w)
	}
	if g.RootClaim, err = word("rootClaim()"); err != nil {
		return g, err
	}
	if g.Timestamp, err = num("l2SequenceNumber()"); err != nil {
		return g, err
	}
	if g.Creator, err = addr("gameCreator()"); err != nil {
		return g, err
	}
	if g.Proposer, err = addr("proposer()"); err != nil {
		return g, err
	}
	if g.Status, err = num("status()"); err != nil {
		return g, err
	}
	respected, err := num("wasRespectedGameTypeWhenCreated()")
	if err != nil {
		return g, err
	}
	g.Respected = respected == 1
	return g, nil
}

// Rollup is the part of rollup.json needed to map an L2 timestamp to a block.
type Rollup struct {
	BlockTime uint64 `json:"block_time"`
	L2ChainID uint64 `json:"l2_chain_id"`
	Genesis   struct {
		L2Time uint64 `json:"l2_time"`
	} `json:"genesis"`
}

// ErrNotABlockTime is returned for a timestamp that no L2 block has.
var ErrNotABlockTime = errors.New("disputegame: timestamp is not an L2 block time")

// BlockAt returns the L2 block number at timestamp.
func (r Rollup) BlockAt(timestamp uint64) (uint64, error) {
	if r.BlockTime == 0 {
		return 0, errors.New("disputegame: rollup config has no block time")
	}
	if timestamp < r.Genesis.L2Time || (timestamp-r.Genesis.L2Time)%r.BlockTime != 0 {
		return 0, fmt.Errorf("%w: %d", ErrNotABlockTime, timestamp)
	}
	return (timestamp - r.Genesis.L2Time) / r.BlockTime, nil
}

// Output is an L2 block opened into what its output root commits to, with storage
// proofs of the L2ToL1MessagePasser for any slots asked for.
type Output struct {
	Number     uint64
	StateRoot  common.Hash
	PasserRoot common.Hash
	BlockHash  common.Hash
	Root       common.Hash    // the version 0 output root
	Slots      []StorageProof // in the order the slots were asked for
}

// StorageProof is one slot of the L2ToL1MessagePasser with its Merkle proof.
type StorageProof struct {
	Key   common.Hash
	Value *hexutil.Big
	Proof []hexutil.Bytes
}

// OutputAt reads block number from an L2 execution client, with eth_getProof of the
// L2ToL1MessagePasser for slots, and computes the output root.
func OutputAt(ctx context.Context, el *rpc.Client, number uint64, slots ...common.Hash) (Output, error) {
	o := Output{Number: number}
	var block struct {
		Hash      common.Hash `json:"hash"`
		StateRoot common.Hash `json:"stateRoot"`
	}
	if err := el.CallContext(ctx, &block, "eth_getBlockByNumber", hexutil.Uint64(number), false); err != nil {
		return o, fmt.Errorf("disputegame: read L2 block %d: %w", number, err)
	}
	if block.Hash == (common.Hash{}) {
		return o, fmt.Errorf("disputegame: L2 block %d not found", number)
	}
	keys := make([]string, len(slots))
	for i, s := range slots {
		keys[i] = s.Hex()
	}
	var proof struct {
		StorageHash  common.Hash    `json:"storageHash"`
		StorageProof []StorageProof `json:"storageProof"`
	}
	if err := el.CallContext(ctx, &proof, "eth_getProof", outputroot.MessagePasser, keys, hexutil.Uint64(number)); err != nil {
		return o, fmt.Errorf("disputegame: eth_getProof at L2 block %d: %w", number, err)
	}
	if len(proof.StorageProof) != len(slots) {
		return o, fmt.Errorf("disputegame: eth_getProof returned %d storage proofs, want %d", len(proof.StorageProof), len(slots))
	}
	o.StateRoot, o.PasserRoot, o.BlockHash = block.StateRoot, proof.StorageHash, block.Hash
	o.Root = outputroot.V0(o.StateRoot, o.PasserRoot, o.BlockHash)
	o.Slots = proof.StorageProof
	return o, nil
}

// SuperRoot is the super root a game at timestamp should claim for this output.
func (r Rollup) SuperRoot(timestamp uint64, o Output) common.Hash {
	return outputroot.SuperV1(timestamp, []outputroot.ChainOutput{{ChainID: r.L2ChainID, Output: o.Root}})
}
