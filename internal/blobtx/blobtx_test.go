package blobtx

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 30, 31, 32, 1000, MaxPayload} {
		payload := bytes.Repeat([]byte{0xab}, n)
		blob, err := Encode(payload)
		if err != nil {
			t.Fatalf("n=%d: encode: %v", n, err)
		}
		got, err := Decode(&blob, n)
		if err != nil {
			t.Fatalf("n=%d: decode: %v", n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("n=%d: round trip changed the payload", n)
		}
	}
}

func TestEncodeKeepsFieldElementsCanonical(t *testing.T) {
	blob, err := Encode(bytes.Repeat([]byte{0xff}, MaxPayload))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fieldElements; i++ {
		if blob[i*bytesPerElement] != 0 {
			t.Fatalf("field element %d starts with %#x, want 0", i, blob[i*bytesPerElement])
		}
	}
}

func TestEncodeRejectsOversizedPayload(t *testing.T) {
	_, err := Encode(make([]byte, MaxPayload+1))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("got %v, want ErrPayloadTooLarge", err)
	}
}

func TestSidecarIsVersion1WithValidCellProofs(t *testing.T) {
	sc, err := Sidecar([]byte("settled blob test"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != types.BlobSidecarVersion1 {
		t.Fatalf("sidecar version %d, want %d", sc.Version, types.BlobSidecarVersion1)
	}
	if len(sc.Proofs) != kzg4844.CellProofsPerBlob {
		t.Fatalf("%d proofs, want %d", len(sc.Proofs), kzg4844.CellProofsPerBlob)
	}
	if err := kzg4844.VerifyCellProofs(sc.Blobs, sc.Commitments, sc.Proofs); err != nil {
		t.Fatalf("cell proofs do not verify: %v", err)
	}
	hashes := sc.BlobHashes()
	if len(hashes) != 1 || hashes[0][0] != 0x01 {
		t.Fatalf("blob hashes %v, want one hash with version byte 0x01", hashes)
	}
}

func TestSidecarProofsFailForAnotherBlob(t *testing.T) {
	sc, err := Sidecar([]byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := Encode([]byte("tampered"))
	if err != nil {
		t.Fatal(err)
	}
	if err := kzg4844.VerifyCellProofs([]kzg4844.Blob{other}, sc.Commitments, sc.Proofs); err == nil {
		t.Fatal("proofs verified for a different blob")
	}
}
