package kmsdev

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/signer"
)

// The test's access keys: made at each run, as bridge-init.sh makes the devnet's.
var (
	adminID, signerID         = "AKIA" + strings.Repeat("A", 16), "AKIA" + strings.Repeat("S", 16)
	adminSecret, signerSecret = rand.Text(), rand.Text()
)

// fixture is the service on a local port with an admin and a signer, each with
// credentials of its own, and the AWS SDK's client for each.
type fixture struct {
	srv                  *Server
	admin, signer, guess *kms.Client
}

func client(url, id, secret string) *kms.Client {
	return kms.New(kms.Options{Region: "eu-central-1", BaseEndpoint: aws.String(url),
		Credentials: credentials.NewStaticCredentialsProvider(id, secret, "")})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kms")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Region: "eu-central-1", Account: "000000000000", Dir: dir, Principals: []Principal{
		{Name: "admin", AccessKeyID: adminID, SecretAccessKey: adminSecret, Actions: []string{"kms:CreateKey", "kms:CreateAlias", "kms:UpdateAlias", "kms:DescribeKey", "kms:GetPublicKey"}},
		{Name: "signer", AccessKeyID: signerID, SecretAccessKey: signerSecret, Actions: []string{"kms:GetPublicKey", "kms:Sign"}, Keys: []string{"alias/payer"}},
	}}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &fixture{srv: srv,
		admin:  client(ts.URL, adminID, adminSecret),
		signer: client(ts.URL, signerID, signerSecret),
		guess:  client(ts.URL, signerID, "a-guess")}
}

func (f *fixture) key(t *testing.T, alias string) string {
	t.Helper()
	ctx := context.Background()
	out, err := f.admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: types.KeySpecEccSecgP256k1, KeyUsage: types.KeyUsageTypeSignVerify})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String(alias), TargetKeyId: out.KeyMetadata.KeyId}); err != nil {
		t.Fatal(err)
	}
	return *out.KeyMetadata.KeyId
}

func code(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// Through the AWS SDK, as against AWS KMS: the signer's key signs Ethereum
// transactions' hashes, each signature recovering to the account of its public key,
// whatever half of the order the service's s falls in.
func TestKMS_signsThroughTheAWSSDK(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.key(t, "alias/payer")
	k, err := signer.NewKMSKey(ctx, f.signer, "alias/payer")
	if err != nil {
		t.Fatal(err)
	}
	half := new(big256).half()
	for i := range 64 {
		hash := crypto.Keccak256Hash([]byte{byte(i)})
		sig, err := k.SignHash(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		pub, err := crypto.SigToPub(hash.Bytes(), sig)
		if err != nil || crypto.PubkeyToAddress(*pub) != k.Address() {
			t.Fatalf("signature %d recovers to %v, %v; want %s", i, pub, err, k.Address())
		}
		if new(big256).set(sig[32:64]).cmp(half) > 0 {
			t.Fatalf("signature %d has s in the upper half", i)
		}
	}
}

// Each principal only what its policy names: the signer cannot make keys or sign with
// another key, the admin cannot sign, a wrong secret or a stale request gets nothing,
// and no call returns a private key.
func TestKMS_eachPrincipalOnlyWhatItsPolicyAllows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	seen := map[string]int{}
	f.srv.Observe = func(operation, principal, outcome string) { seen[operation+" "+principal+" "+outcome]++ }
	f.key(t, "alias/payer")
	other := f.key(t, "alias/other")
	digest := make([]byte, 32)
	sign := func(c *kms.Client, keyID string) error {
		_, err := c.Sign(ctx, &kms.SignInput{KeyId: aws.String(keyID), Message: digest, MessageType: types.MessageTypeDigest, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256})
		return err
	}
	for _, c := range []struct {
		name, want string
		err        error
	}{
		{"the signer makes a key", "AccessDeniedException", func() error {
			_, err := f.signer.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: types.KeySpecEccSecgP256k1, KeyUsage: types.KeyUsageTypeSignVerify})
			return err
		}()},
		{"the signer signs with another key", "AccessDeniedException", sign(f.signer, other)},
		{"the admin signs", "AccessDeniedException", sign(f.admin, "alias/payer")},
		{"a wrong secret", "InvalidSignatureException", sign(f.guess, "alias/payer")},
		{"a key of another spec", "UnsupportedOperationException", func() error {
			_, err := f.admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: types.KeySpecEccNistP256, KeyUsage: types.KeyUsageTypeSignVerify})
			return err
		}()},
		{"a key that does not exist", "NotFoundException", sign(f.signer, "alias/nobody")},
		{"a message, not a digest", "ValidationException", func() error {
			_, err := f.signer.Sign(ctx, &kms.SignInput{KeyId: aws.String("alias/payer"), Message: []byte("hello"), MessageType: types.MessageTypeRaw, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256})
			return err
		}()},
		{"an alias taken", "AlreadyExistsException", func() error {
			_, err := f.admin.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String("alias/payer"), TargetKeyId: aws.String(other)})
			return err
		}()},
	} {
		if got := code(c.err); got != c.want {
			t.Errorf("%s: %v, want %s", c.name, c.err, c.want)
		}
	}
	// A request signed ten minutes ago is refused.
	f.srv.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if got := code(sign(f.signer, "alias/payer")); got != "InvalidSignatureException" {
		t.Errorf("a stale request: %s, want InvalidSignatureException", got)
	}
	f.srv.Now = nil
	// Each call is reported: a caller who is not who it says, or asks what its policy
	// does not allow, as denied.
	want := map[string]int{
		"CreateKey admin ok": 2, "CreateAlias admin ok": 2, "CreateKey signer denied": 1, "Sign signer denied": 1, "Sign admin denied": 1,
		"Sign  denied": 2, "CreateKey admin refused": 1, "Sign signer refused": 2, "CreateAlias admin refused": 1,
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("observed %v, want %v", seen, want)
	}
	// The keys are kept for the service only.
	info, err := os.Stat(filepath.Join(f.srv.Dir, stateFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file: %v, %v; want 600", info.Mode().Perm(), err)
	}
}

// A key is rotated by moving its alias: the signer, which may use only alias/payer,
// signs with the new key and no longer with the old; only the admin moves an alias.
func TestKMS_aKeyIsRotatedByMovingItsAlias(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	old := f.key(t, "alias/payer")
	made, err := f.admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: types.KeySpecEccSecgP256k1, KeyUsage: types.KeyUsageTypeSignVerify})
	if err != nil {
		t.Fatal(err)
	}
	move := func(c *kms.Client, target string) error {
		_, err := c.UpdateAlias(ctx, &kms.UpdateAliasInput{AliasName: aws.String("alias/payer"), TargetKeyId: aws.String(target)})
		return err
	}
	if got := code(move(f.signer, *made.KeyMetadata.KeyId)); got != "AccessDeniedException" {
		t.Fatalf("the signer moves the alias: %s, want AccessDeniedException", got)
	}
	if err := move(f.admin, *made.KeyMetadata.KeyId); err != nil {
		t.Fatal(err)
	}
	if got := code(func() error {
		_, err := f.admin.UpdateAlias(ctx, &kms.UpdateAliasInput{AliasName: aws.String("alias/nobody"), TargetKeyId: made.KeyMetadata.KeyId})
		return err
	}()); got != "NotFoundException" {
		t.Fatalf("an alias that does not exist: %s, want NotFoundException", got)
	}
	k, err := signer.NewKMSKey(ctx, f.signer, "alias/payer")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := f.admin.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: made.KeyMetadata.KeyId})
	if err != nil {
		t.Fatal(err)
	}
	want, err := signer.ParseSecp256k1SPKI(pub.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Address() != common.BytesToAddress(crypto.Keccak256(want[1:])[12:]) {
		t.Fatal("after the move, the signer's account is not the new key's")
	}
	if _, err := k.SignHash(ctx, common.Hash{1}); err != nil {
		t.Fatal(err)
	}
	if got := code(func() error {
		_, err := f.signer.Sign(ctx, &kms.SignInput{KeyId: aws.String(old), Message: make([]byte, 32), MessageType: types.MessageTypeDigest, SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256})
		return err
	}()); got != "AccessDeniedException" {
		t.Fatalf("the signer signs with the old key: %s, want AccessDeniedException", got)
	}
}

// The keys stay across a restart: a new server on the same folder signs for the same
// account.
func TestKMS_keysStayAcrossARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.key(t, "alias/payer")
	before, err := signer.NewKMSKey(ctx, f.signer, "alias/payer")
	if err != nil {
		t.Fatal(err)
	}
	again := &Server{Region: f.srv.Region, Account: f.srv.Account, Dir: f.srv.Dir, Principals: f.srv.Principals}
	ts := httptest.NewServer(again)
	defer ts.Close()
	after, err := signer.NewKMSKey(ctx, client(ts.URL, signerID, signerSecret), "alias/payer")
	if err != nil {
		t.Fatal(err)
	}
	if before.Address() != after.Address() || before.Address() == (common.Address{}) {
		t.Fatalf("%s before, %s after the restart", before.Address(), after.Address())
	}
}
