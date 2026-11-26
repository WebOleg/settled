// Package chaincall reads single-word values from contracts: getters that take no
// arguments and return one address or one unsigned integer. It is enough to check a
// deployment against what was intended without generated bindings.
package chaincall

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Caller is the part of ethclient.Client that this package needs.
type Caller interface {
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
}

// ErrNotAWord is returned when a call does not return exactly one 32-byte word.
var ErrNotAWord = errors.New("chaincall: return value is not one 32-byte word")

// ErrNotAnAddress is returned when a word has bits set above the low 20 bytes.
var ErrNotAnAddress = errors.New("chaincall: word is not an address")

// Selector returns the 4-byte function selector of a signature such as "owner()".
func Selector(signature string) []byte {
	return crypto.Keccak256([]byte(signature))[:4]
}

// Word calls a no-argument getter on contract at the latest block and returns its
// single 32-byte return value.
func Word(ctx context.Context, c Caller, contract common.Address, signature string) (common.Hash, error) {
	out, err := c.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: Selector(signature)}, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("chaincall: %s on %s: %w", signature, contract, err)
	}
	if len(out) != 32 {
		return common.Hash{}, fmt.Errorf("%w: %s on %s returned %d bytes", ErrNotAWord, signature, contract, len(out))
	}
	return common.BytesToHash(out), nil
}

// AddressOf decodes a word that holds an address, refusing one with high bits set.
func AddressOf(w common.Hash) (common.Address, error) {
	for _, b := range w[:12] {
		if b != 0 {
			return common.Address{}, fmt.Errorf("%w: %s", ErrNotAnAddress, w)
		}
	}
	return common.BytesToAddress(w[12:]), nil
}

// UintOf decodes a word as an unsigned integer.
func UintOf(w common.Hash) *big.Int {
	return new(big.Int).SetBytes(w[:])
}

// AddressWord returns the word that holds address a, as stored by contracts that keep an
// address in a bytes32 field (for example SystemConfig.batcherHash).
func AddressWord(a common.Address) common.Hash {
	return common.BytesToHash(a[:])
}
