// Package p2pid derives the libp2p peer ID that op-node announces for its peer-to-peer
// key, so a node can be given the full address of a static peer before either starts.
//
// op-node uses secp256k1 keys. The peer ID is the base58btc form of an identity
// multihash over the protobuf-encoded public key: key type 2 (secp256k1) and the
// 33-byte compressed key. Such IDs start with "16Uiu2HA".
package p2pid

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

// CompressedKeyLen is the length of a compressed secp256k1 public key.
const CompressedKeyLen = 33

// ErrBadPublicKey is returned for a public key that is not 33 bytes with prefix 2 or 3.
var ErrBadPublicKey = errors.New("p2pid: not a compressed secp256k1 public key")

// FromPrivateKeyHex returns the peer ID for a private key written as hex, the format
// op-node reads from --p2p.priv.path. Surrounding white space and a 0x prefix are
// allowed.
func FromPrivateKeyHex(s string) (string, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil {
		return "", fmt.Errorf("p2pid: parse private key: %w", err)
	}
	return FromCompressedPublicKey(crypto.CompressPubkey(&key.PublicKey))
}

// FromCompressedPublicKey returns the peer ID for a compressed secp256k1 public key.
func FromCompressedPublicKey(pub []byte) (string, error) {
	if len(pub) != CompressedKeyLen || (pub[0] != 2 && pub[0] != 3) {
		return "", ErrBadPublicKey
	}
	// Protobuf PublicKey: field 1 (varint) Type = 2, field 2 (bytes) Data = pub.
	encoded := append([]byte{0x08, 0x02, 0x12, CompressedKeyLen}, pub...)
	// Keys of at most 42 bytes are not hashed: identity multihash, code 0x00, then length.
	multihash := append([]byte{0x00, byte(len(encoded))}, encoded...)
	return base58(multihash), nil
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58 encodes b in the Bitcoin alphabet; each leading zero byte becomes a '1'.
func base58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	radix, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
