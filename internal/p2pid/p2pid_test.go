package p2pid

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Vectors computed independently: the public key from secp256k1 point multiplication,
// the peer ID from the encoding described in the package comment.
var vectors = []struct {
	priv, pub, id string
}{
	{
		priv: "0000000000000000000000000000000000000000000000000000000000000001",
		pub:  "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		id:   "16Uiu2HAm3cuhhRL2msUuLF62KRSfneFDx94RsuouyW25Ho42cFMq",
	},
	{
		priv: "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318",
		pub:  "024e3b81af9c2234cad09d679ce6035ed1392347ce64ce405f5dcd36228a25de6e",
		id:   "16Uiu2HAkzh4QdxB7ijzVnwd6JFNNe4bcr1JBwYUypXe3pX5BRpqK",
	},
}

func TestFromCompressedPublicKey(t *testing.T) {
	for _, v := range vectors {
		pub, err := hex.DecodeString(v.pub)
		if err != nil {
			t.Fatal(err)
		}
		got, err := FromCompressedPublicKey(pub)
		if err != nil {
			t.Fatalf("%s: %v", v.pub, err)
		}
		if got != v.id {
			t.Errorf("%s: got %s, want %s", v.pub, got, v.id)
		}
	}
}

func TestFromPrivateKeyHex(t *testing.T) {
	for _, v := range vectors {
		for _, in := range []string{v.priv, "0x" + v.priv, v.priv + "\n"} {
			got, err := FromPrivateKeyHex(in)
			if err != nil {
				t.Fatalf("%q: %v", in, err)
			}
			if got != v.id {
				t.Errorf("%q: got %s, want %s", in, got, v.id)
			}
		}
	}
	if _, err := FromPrivateKeyHex("not hex"); err == nil {
		t.Error("accepted a key that is not hex")
	}
}

func TestRejectsBadPublicKey(t *testing.T) {
	for _, pub := range [][]byte{nil, make([]byte, 32), append([]byte{4}, make([]byte, 32)...)} {
		if _, err := FromCompressedPublicKey(pub); !errors.Is(err, ErrBadPublicKey) {
			t.Errorf("%x: got %v, want ErrBadPublicKey", pub, err)
		}
	}
}

func TestPrefix(t *testing.T) {
	for _, v := range vectors {
		if !strings.HasPrefix(v.id, "16Uiu2HA") {
			t.Errorf("%s does not start with 16Uiu2HA", v.id)
		}
	}
}

func TestBase58(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"hello world":  "StV1DL6CwTryKyV",
		"\x00\x00\x01": "112",
	}
	for in, want := range cases {
		if got := base58([]byte(in)); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
}
