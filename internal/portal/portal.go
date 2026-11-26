// Package portal encodes the calls that move ETH between the L1 and the L2 through the
// OP Stack contracts: deposits through OptimismPortal, withdrawals started at the
// L2ToL1MessagePasser predeploy, and their proof and finalization on the L1. It also
// computes the withdrawal hash and the storage slot that records it, which a withdrawal
// proof is about.
package portal

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The functions used, from OptimismPortal2 (op-contracts v9, version 6.1.0) and
// L2ToL1MessagePasser. Withdrawal transactions are a tuple, as in the contracts.
const abiJSON = `[
 {"type":"function","name":"depositTransaction","stateMutability":"payable","inputs":[
  {"name":"_to","type":"address"},{"name":"_value","type":"uint256"},{"name":"_gasLimit","type":"uint64"},
  {"name":"_isCreation","type":"bool"},{"name":"_data","type":"bytes"}],"outputs":[]},
 {"type":"function","name":"initiateWithdrawal","stateMutability":"payable","inputs":[
  {"name":"_target","type":"address"},{"name":"_gasLimit","type":"uint256"},{"name":"_data","type":"bytes"}],"outputs":[]},
 {"type":"function","name":"proveWithdrawalTransaction","stateMutability":"nonpayable","inputs":[
  {"name":"_tx","type":"tuple","components":[
   {"name":"nonce","type":"uint256"},{"name":"sender","type":"address"},{"name":"target","type":"address"},
   {"name":"value","type":"uint256"},{"name":"gasLimit","type":"uint256"},{"name":"data","type":"bytes"}]},
  {"name":"_disputeGameIndex","type":"uint256"},
  {"name":"_outputRootProof","type":"tuple","components":[
   {"name":"version","type":"bytes32"},{"name":"stateRoot","type":"bytes32"},
   {"name":"messagePasserStorageRoot","type":"bytes32"},{"name":"latestBlockhash","type":"bytes32"}]},
  {"name":"_withdrawalProof","type":"bytes[]"}],"outputs":[]},
 {"type":"function","name":"finalizeWithdrawalTransaction","stateMutability":"nonpayable","inputs":[
  {"name":"_tx","type":"tuple","components":[
   {"name":"nonce","type":"uint256"},{"name":"sender","type":"address"},{"name":"target","type":"address"},
   {"name":"value","type":"uint256"},{"name":"gasLimit","type":"uint256"},{"name":"data","type":"bytes"}]}],"outputs":[]},
 {"type":"function","name":"finalizedWithdrawals","stateMutability":"view","inputs":[
  {"name":"","type":"bytes32"}],"outputs":[{"name":"","type":"bool"}]}
]`

var parsed = mustParse()

func mustParse() abi.ABI {
	a, err := abi.JSON(strings.NewReader(abiJSON))
	if err != nil {
		panic(fmt.Sprintf("portal: parse ABI: %v", err))
	}
	return a
}

// WithdrawalTx is a withdrawal as the contracts see it. Field names match the ABI tuple.
type WithdrawalTx struct {
	Nonce    *big.Int
	Sender   common.Address
	Target   common.Address
	Value    *big.Int
	GasLimit *big.Int
	Data     []byte
}

// OutputRootProof opens an output root into the values it commits to.
type OutputRootProof struct {
	Version                  [32]byte
	StateRoot                [32]byte
	MessagePasserStorageRoot [32]byte
	LatestBlockhash          [32]byte
}

// Deposit is the call data of OptimismPortal.depositTransaction for a plain ETH deposit
// to an address on the L2.
func Deposit(to common.Address, value *big.Int, gasLimit uint64) ([]byte, error) {
	return parsed.Pack("depositTransaction", to, value, gasLimit, false, []byte{})
}

// InitiateWithdrawal is the call data of L2ToL1MessagePasser.initiateWithdrawal.
func InitiateWithdrawal(target common.Address, gasLimit *big.Int, data []byte) ([]byte, error) {
	return parsed.Pack("initiateWithdrawal", target, gasLimit, data)
}

// Prove is the call data of OptimismPortal.proveWithdrawalTransaction.
func Prove(tx WithdrawalTx, gameIndex *big.Int, proof OutputRootProof, storageProof [][]byte) ([]byte, error) {
	return parsed.Pack("proveWithdrawalTransaction", tx, gameIndex, proof, storageProof)
}

// Finalize is the call data of OptimismPortal.finalizeWithdrawalTransaction.
func Finalize(tx WithdrawalTx) ([]byte, error) {
	return parsed.Pack("finalizeWithdrawalTransaction", tx)
}

// FinalizedWithdrawals is the call data of OptimismPortal.finalizedWithdrawals(hash).
func FinalizedWithdrawals(hash common.Hash) ([]byte, error) {
	return parsed.Pack("finalizedWithdrawals", hash)
}

// Hash is the withdrawal hash: keccak256(abi.encode(nonce, sender, target, value,
// gasLimit, data)). The L1 marks a withdrawal proven and finalized under this hash.
func Hash(tx WithdrawalTx) (common.Hash, error) {
	encoded, err := parsed.Methods["finalizeWithdrawalTransaction"].Inputs.Pack(tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("portal: encode withdrawal: %w", err)
	}
	// Inputs.Pack encodes the tuple as the single argument of a call: an offset word
	// first, then the tuple. abi.encode of the fields is the tuple alone.
	return crypto.Keccak256Hash(encoded[32:]), nil
}

// SentMessageSlot is the storage slot of the L2ToL1MessagePasser that is set to true for
// a withdrawal: sentMessages[hash], a mapping at slot 0.
func SentMessageSlot(hash common.Hash) common.Hash {
	var slot common.Hash
	return crypto.Keccak256Hash(hash[:], slot[:])
}
