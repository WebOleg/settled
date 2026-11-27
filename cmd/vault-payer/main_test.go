package main

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/ethereum/go-ethereum/common"

	"github.com/WebOleg/settled/internal/kmsdev"
	"github.com/WebOleg/settled/internal/signer"
)

func TestEnsureKey_madeOnceInTheKMSAndTheSignerSignsAsItsAccount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kms")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	adminID, signerID := "AKIA"+strings.Repeat("A", 16), "AKIA"+strings.Repeat("S", 16)
	adminSecret, signerSecret := rand.Text(), rand.Text()
	rotationID, rotationSecret := "AKIA"+strings.Repeat("R", 16), rand.Text()
	srv := httptest.NewServer(&kmsdev.Server{Region: "eu-central-1", Account: "000000000000", Dir: dir, Principals: []kmsdev.Principal{
		{Name: "admin", AccessKeyID: adminID, SecretAccessKey: adminSecret,
			Actions: []string{"kms:CreateKey", "kms:CreateAlias", "kms:UpdateAlias", "kms:DescribeKey", "kms:GetPublicKey"}},
		{Name: "withdrawal-signer", AccessKeyID: signerID, SecretAccessKey: signerSecret,
			Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer"}},
		{Name: "key-rotation", AccessKeyID: rotationID, SecretAccessKey: rotationSecret,
			Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer-retiring"}},
	}})
	defer srv.Close()
	client := func(id, secret string) *kms.Client {
		return kms.New(kms.Options{Region: "eu-central-1", BaseEndpoint: aws.String(srv.URL), Credentials: credentials.NewStaticCredentialsProvider(id, secret, "")})
	}
	ctx := context.Background()
	admin := client(adminID, adminSecret)

	id, account, err := ensureKey(ctx, admin, false)
	if err != nil {
		t.Fatal(err)
	}
	again, same, err := ensureKey(ctx, admin, false)
	if err != nil {
		t.Fatal(err)
	}
	if again != id || same != account {
		t.Fatalf("run again: key %s, account %s; want the same %s, %s", again, same, id, account)
	}
	// Rotated, alias/payer names a new key, and the signer signs as its account.
	rotated, moved, err := ensureKey(ctx, admin, true)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == id || moved == account {
		t.Fatalf("rotated: key %s, account %s; want new ones", rotated, moved)
	}
	// The key it replaced is named alias/payer-retiring, which key rotation, and only it,
	// signs with, to empty it; key rotation cannot sign with the payer's key.
	rotation := client(rotationID, rotationSecret)
	old, err := signer.NewKMSKey(ctx, rotation, "alias/payer-retiring")
	if err != nil || old.Address() != account {
		t.Fatalf("alias/payer-retiring: %v, %v; want the replaced key's account %s", old, err, account)
	}
	if _, err := old.SignHash(ctx, common.Hash{1}); err != nil {
		t.Fatalf("key rotation signs with the retiring key: %v", err)
	}
	if _, err := signer.NewKMSKey(ctx, client(signerID, signerSecret), "alias/payer-retiring"); err == nil {
		t.Fatal("the signer reads the retiring key")
	}
	if _, err := rotation.Sign(ctx, &kms.SignInput{KeyId: aws.String("alias/payer"), Message: make([]byte, 32), MessageType: "DIGEST", SigningAlgorithm: "ECDSA_SHA_256"}); err == nil {
		t.Fatal("key rotation signed with the payer's key")
	}
	// Rotated again, alias/payer-retiring moves on to the key just replaced.
	if _, _, err := ensureKey(ctx, admin, true); err != nil {
		t.Fatal(err)
	}
	if again, err := signer.NewKMSKey(ctx, rotation, "alias/payer-retiring"); err != nil || again.Address() != moved {
		t.Fatalf("rotated twice, alias/payer-retiring: %v, %v; want %s", again, err, moved)
	}
	if id, account, err = ensureKey(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	k, err := signer.NewKMSKey(ctx, client(signerID, signerSecret), "alias/payer")
	if err != nil {
		t.Fatal(err)
	}
	if k.Address() != account {
		t.Fatalf("the signer's account %s, want the payer's %s", k.Address(), account)
	}
	// The admin makes and names keys and cannot sign; the signer signs and cannot make one.
	if _, err := admin.Sign(ctx, &kms.SignInput{KeyId: aws.String("alias/payer"), Message: make([]byte, 32), MessageType: "DIGEST", SigningAlgorithm: "ECDSA_SHA_256"}); err == nil {
		t.Fatal("the admin signed")
	}
	if _, err := client(signerID, signerSecret).CreateKey(ctx, &kms.CreateKeyInput{KeySpec: "ECC_SECG_P256K1", KeyUsage: "SIGN_VERIFY"}); err == nil {
		t.Fatal("the signer made a key")
	}
}
