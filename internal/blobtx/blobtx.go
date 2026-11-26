// Package blobtx builds the blob part of an EIP-4844 transaction in the format used
// since Fusaka: a version 1 sidecar that carries cell proofs (EIP-7594) instead of one
// proof per blob.
package blobtx

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
)

const (
	// A blob is 4096 field elements of 32 bytes. Each element must be smaller than the
	// BLS12-381 scalar field modulus; keeping its first byte zero guarantees that, so
	// 31 bytes per element carry data.
	fieldElements     = 4096
	bytesPerElement   = 32
	payloadPerElement = 31

	// MaxPayload is the largest payload one blob carries with this encoding.
	MaxPayload = fieldElements * payloadPerElement
)

// ErrPayloadTooLarge is returned when the payload does not fit into one blob.
var ErrPayloadTooLarge = errors.New("blobtx: payload does not fit into one blob")

// Encode places payload into a blob, 31 bytes per field element, leaving the first
// byte of every element zero. The rest of the blob is zero.
func Encode(payload []byte) (kzg4844.Blob, error) {
	var blob kzg4844.Blob
	if len(payload) > MaxPayload {
		return blob, fmt.Errorf("%w: %d bytes, max %d", ErrPayloadTooLarge, len(payload), MaxPayload)
	}
	for i := 0; i*payloadPerElement < len(payload); i++ {
		start := i * payloadPerElement
		end := min(start+payloadPerElement, len(payload))
		copy(blob[i*bytesPerElement+1:], payload[start:end])
	}
	return blob, nil
}

// Decode reverses Encode and returns the first n payload bytes.
func Decode(blob *kzg4844.Blob, n int) ([]byte, error) {
	if n < 0 || n > MaxPayload {
		return nil, fmt.Errorf("blobtx: decode length %d outside 0..%d", n, MaxPayload)
	}
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		take := min(payloadPerElement, n-len(out))
		start := i*bytesPerElement + 1
		out = append(out, blob[start:start+take]...)
	}
	return out, nil
}

// Sidecar encodes payload into one blob and returns a version 1 sidecar: the blob, its
// KZG commitment and its cell proofs.
func Sidecar(payload []byte) (*types.BlobTxSidecar, error) {
	blob, err := Encode(payload)
	if err != nil {
		return nil, err
	}
	commitment, err := kzg4844.BlobToCommitment(&blob)
	if err != nil {
		return nil, fmt.Errorf("blobtx: commit to blob: %w", err)
	}
	proofs, err := kzg4844.ComputeCellProofs(&blob)
	if err != nil {
		return nil, fmt.Errorf("blobtx: compute cell proofs: %w", err)
	}
	return types.NewBlobTxSidecar(
		types.BlobSidecarVersion1,
		[]kzg4844.Blob{blob},
		[]kzg4844.Commitment{commitment},
		proofs,
	), nil
}
