package signer

import (
	"context"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// KMSKey is a key held in a KMS that speaks the AWS KMS API: an asymmetric
// ECC_SECG_P256K1 key with SIGN_VERIFY usage, which never leaves it. The signer may
// only read its public key and sign a digest with it (kms:GetPublicKey, kms:Sign).
// The KMS returns an ASN.1 DER signature; Ethereum wants r, s with s in the lower half
// of the curve's order, and the recovery id, which is found by recovering the public
// key both ways.
type KMSKey struct {
	client  *kms.Client
	keyID   string
	public  []byte // uncompressed, 65 bytes
	address common.Address
}

// The OIDs of a secp256k1 public key in a SubjectPublicKeyInfo.
var (
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidSecp256k1   = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
	// secpN is the order of secp256k1, and halfN half of it.
	secpN  = crypto.S256().Params().N
	halfN  = new(big.Int).Rsh(secpN, 1)
	errDER = errors.New("signer: not a DER ECDSA signature")
)

// NewKMSKey reads the public key of keyID from the KMS and derives the account.
func NewKMSKey(ctx context.Context, client *kms.Client, keyID string) (*KMSKey, error) {
	out, err := client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("signer: the KMS public key of %s: %w", keyID, err)
	}
	if out.KeySpec != types.KeySpecEccSecgP256k1 || out.KeyUsage != types.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("signer: KMS key %s is %s for %s, want ECC_SECG_P256K1 for SIGN_VERIFY", keyID, out.KeySpec, out.KeyUsage)
	}
	public, err := ParseSecp256k1SPKI(out.PublicKey)
	if err != nil {
		return nil, err
	}
	pub, err := crypto.UnmarshalPubkey(public)
	if err != nil {
		return nil, fmt.Errorf("signer: the KMS public key is not on secp256k1: %w", err)
	}
	return &KMSKey{client: client, keyID: keyID, public: public, address: crypto.PubkeyToAddress(*pub)}, nil
}

func (k *KMSKey) Address() common.Address { return k.address }

// SignHash asks the KMS to sign the digest, and turns its answer into Ethereum's form.
func (k *KMSKey) SignHash(ctx context.Context, hash common.Hash) ([]byte, error) {
	out, err := k.client.Sign(ctx, &kms.SignInput{
		KeyId: aws.String(k.keyID), Message: hash.Bytes(),
		MessageType: types.MessageTypeDigest, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("signer: KMS sign: %w", err)
	}
	return EthereumSignature(hash, out.Signature, k.public)
}

// ParseSecp256k1SPKI reads an X.509 SubjectPublicKeyInfo of a secp256k1 key, as the KMS
// returns it, and gives the uncompressed point. The standard library does not know the
// curve, so the structure is read here, and anything else is refused.
func ParseSecp256k1SPKI(der []byte) ([]byte, error) {
	var spki struct {
		Algorithm struct {
			Algorithm  asn1.ObjectIdentifier
			Parameters asn1.ObjectIdentifier
		}
		PublicKey asn1.BitString
	}
	rest, err := asn1.Unmarshal(der, &spki)
	if err != nil || len(rest) != 0 {
		return nil, errors.New("signer: the public key is not a SubjectPublicKeyInfo")
	}
	if !spki.Algorithm.Algorithm.Equal(oidECPublicKey) || !spki.Algorithm.Parameters.Equal(oidSecp256k1) {
		return nil, fmt.Errorf("signer: the public key is %v on %v, want an EC key on secp256k1", spki.Algorithm.Algorithm, spki.Algorithm.Parameters)
	}
	point := spki.PublicKey.RightAlign()
	if spki.PublicKey.BitLength != 65*8 || len(point) != 65 || point[0] != 4 {
		return nil, errors.New("signer: the public key is not an uncompressed point")
	}
	return point, nil
}

// EthereumSignature turns a DER ECDSA signature of hash into the 65-byte r || s || v
// Ethereum takes: s in the lower half of the order (EIP-2), v the recovery id that
// gives back public.
func EthereumSignature(hash common.Hash, der, public []byte) ([]byte, error) {
	var sig struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil || len(rest) != 0 || sig.R == nil || sig.S == nil {
		return nil, errDER
	}
	if sig.R.Sign() <= 0 || sig.S.Sign() <= 0 || sig.R.Cmp(secpN) >= 0 || sig.S.Cmp(secpN) >= 0 {
		return nil, errDER
	}
	s := new(big.Int).Set(sig.S)
	if s.Cmp(halfN) > 0 {
		s.Sub(secpN, s)
	}
	out := make([]byte, 65)
	sig.R.FillBytes(out[:32])
	s.FillBytes(out[32:64])
	for v := byte(0); v < 2; v++ {
		out[64] = v
		got, err := crypto.Ecrecover(hash.Bytes(), out)
		if err == nil && string(got) == string(public) {
			return out, nil
		}
	}
	return nil, errors.New("signer: the KMS signature does not recover to its public key")
}
