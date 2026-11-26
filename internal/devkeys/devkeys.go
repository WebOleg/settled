// Package devkeys derives Ethereum keys from a BIP39 mnemonic along the standard path
// m/44'/60'/0'/0/<index>. It exists only to use the funded test accounts of the local
// devnet, whose mnemonic is public. It must never be used for keys that hold value.
package devkeys

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

const hardened = uint32(0x80000000)

// ErrInvalidKey is returned when a derivation step produces a key outside the curve
// order. BIP32 says to skip that index; for a devnet tool, failing is the right answer.
var ErrInvalidKey = errors.New("devkeys: derived key is invalid for this index")

// FromMnemonic returns the private key at m/44'/60'/0'/0/index.
// The mnemonic is used as given: the devnet mnemonic is lowercase ASCII, for which the
// NFKD normalisation required by BIP39 changes nothing.
func FromMnemonic(mnemonic string, index uint32) (*ecdsa.PrivateKey, error) {
	words := strings.Fields(mnemonic)
	if len(words) == 0 {
		return nil, errors.New("devkeys: empty mnemonic")
	}
	seed, err := pbkdf2.Key(sha512.New, strings.Join(words, " "), []byte("mnemonic"), 2048, 64)
	if err != nil {
		return nil, fmt.Errorf("devkeys: seed from mnemonic: %w", err)
	}

	key, chain := master(seed)
	for _, step := range []uint32{44 | hardened, 60 | hardened, 0 | hardened, 0, index} {
		key, chain, err = child(key, chain, step)
		if err != nil {
			return nil, err
		}
	}
	priv, err := crypto.ToECDSA(key)
	if err != nil {
		return nil, fmt.Errorf("devkeys: private key from bytes: %w", err)
	}
	return priv, nil
}

// master implements the BIP32 master key generation.
func master(seed []byte) (key, chain []byte) {
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	return sum[:32], sum[32:]
}

// child implements BIP32 private parent to private child derivation.
func child(key, chain []byte, index uint32) ([]byte, []byte, error) {
	data := make([]byte, 0, 37)
	if index >= hardened {
		data = append(data, 0x00)
		data = append(data, key...)
	} else {
		priv, err := crypto.ToECDSA(key)
		if err != nil {
			return nil, nil, fmt.Errorf("devkeys: parent key: %w", err)
		}
		data = append(data, crypto.CompressPubkey(&priv.PublicKey)...)
	}
	data = binary.BigEndian.AppendUint32(data, index)

	mac := hmac.New(sha512.New, chain)
	mac.Write(data)
	sum := mac.Sum(nil)

	n := crypto.S256().Params().N
	il := new(big.Int).SetBytes(sum[:32])
	if il.Cmp(n) >= 0 {
		return nil, nil, ErrInvalidKey
	}
	k := il.Add(il, new(big.Int).SetBytes(key))
	k.Mod(k, n)
	if k.Sign() == 0 {
		return nil, nil, ErrInvalidKey
	}
	out := make([]byte, 32)
	k.FillBytes(out)
	return out, sum[32:], nil
}
