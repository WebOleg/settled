package kmsdev

import (
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"
)

// big256 is a little help to compare 32-byte numbers in the tests.
type big256 struct{ big.Int }

func (b *big256) set(x []byte) *big256 { b.SetBytes(x); return b }

func (b *big256) half() *big256 { b.Rsh(crypto.S256().Params().N, 1); return b }

func (b *big256) cmp(o *big256) int { return b.Cmp(&o.Int) }
