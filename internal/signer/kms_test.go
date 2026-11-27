package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// What a KMS returns is read strictly: a public key on another curve, a DER that is not
// a signature or carries more, an r or s out of range, a signature by another key.
func TestKMSAnswers_anythingButASecp256k1KeyAndItsSignatureIsRefused(t *testing.T) {
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&p256.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSecp256k1SPKI(der); err == nil {
		t.Error("a P-256 public key: accepted")
	}
	if _, err := ParseSecp256k1SPKI([]byte{0x30, 0x00}); err == nil {
		t.Error("an empty sequence: accepted")
	}

	key, _ := crypto.GenerateKey()
	public := crypto.FromECDSAPub(&key.PublicKey)
	hash := crypto.Keccak256Hash([]byte("x"))
	sig, _ := crypto.Sign(hash.Bytes(), key)
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64])
	good, _ := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if _, err := EthereumSignature(hash, good, public); err != nil {
		t.Fatalf("a good signature: %v", err)
	}
	other, _ := crypto.GenerateKey()
	for name, der := range map[string][]byte{
		"trailing bytes": append(append([]byte{}, good...), 0),
		"r zero":         must(asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(0), s})),
		"s the order":    must(asn1.Marshal(struct{ R, S *big.Int }{r, secpN})),
		"not a sequence": {0x02, 0x01, 0x01},
	} {
		if _, err := EthereumSignature(hash, der, public); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := EthereumSignature(hash, good, crypto.FromECDSAPub(&other.PublicKey)); err == nil {
		t.Error("a signature by another key: accepted")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
