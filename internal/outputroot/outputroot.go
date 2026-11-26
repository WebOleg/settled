// Package outputroot computes the roots an OP Stack proposer commits to, from data any
// execution client returns, so a proposal can be checked without trusting the rollup
// node that produced it.
//
// An output root (version 0) is keccak256 of the version, the block's state root, the
// storage root of the L2ToL1MessagePasser contract at that block and the block hash. A
// super root (version 1) is keccak256 of the version byte, the timestamp as 8 bytes
// big-endian, then for each chain in ascending chain id order the chain id as 32 bytes
// and its output root.
package outputroot

import (
	"encoding/binary"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// MessagePasser is the L2ToL1MessagePasser predeploy. Withdrawals are stored in it, and
// its storage root is part of every output root, which is what lets a withdrawal be
// proven on the L1.
var MessagePasser = common.HexToAddress("0x4200000000000000000000000000000000000016")

// V0 returns the version 0 output root of an L2 block.
func V0(stateRoot, messagePasserStorageRoot, blockHash common.Hash) common.Hash {
	var version common.Hash // 32 zero bytes
	return crypto.Keccak256Hash(version[:], stateRoot[:], messagePasserStorageRoot[:], blockHash[:])
}

// ChainOutput is one chain's output root inside a super root.
type ChainOutput struct {
	ChainID uint64
	Output  common.Hash
}

// SuperV1 returns the version 1 super root at a timestamp. The chains are encoded in
// ascending chain id order whatever order they are given in.
func SuperV1(timestamp uint64, chains []ChainOutput) common.Hash {
	sorted := append([]ChainOutput(nil), chains...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ChainID < sorted[j].ChainID })
	buf := make([]byte, 0, 1+8+64*len(sorted))
	buf = append(buf, 1)
	buf = binary.BigEndian.AppendUint64(buf, timestamp)
	for _, c := range sorted {
		id := common.BigToHash(new(big.Int).SetUint64(c.ChainID))
		buf = append(buf, id[:]...)
		buf = append(buf, c.Output[:]...)
	}
	return crypto.Keccak256Hash(buf)
}
