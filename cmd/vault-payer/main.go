// Command vault-payer gives the withdrawal payer its key on the devnet, the way a key
// is given to a hot wallet in production: made in the KMS, where it stays, and given the
// vault's role through the timelock that administers the vault.
//
//  1. the key is made in the KMS (kms-dev), once, by its admin, and named alias/payer;
//     the admin reads its public key, and so its account, and nothing else;
//  2. if that account does not hold the vault's operator role, the timelock proposer
//     schedules grantRole(OPERATOR_ROLE, payer) and, after the timelock's delay,
//     executes it; the account is topped up with ETH for gas on the L2;
//  3. every key the payer held before (the record's former payer, and a key in a file
//     from before the KMS) has its role taken away the same way, only once the new one
//     holds it, so withdrawals are never left without a payer; then the ETH of each one
//     that can still sign, holding no role, is moved to the new account (internal/sweep):
//     a key in a file, and the key a rotation replaced, named alias/payer-retiring, which
//     only the key-rotation principal may sign with;
//  4. the record names the payer's account and its key in the KMS, which the withdrawal
//     signer checks before it signs; no copy of a key is left for the signer's
//     container;
//  5. the payer holds the operator role and nothing else; each former key holds none.
//
// With -rotate, a new key is made in the KMS, the one alias/payer names is named
// alias/payer-retiring, alias/payer moves to the new one, and the steps go on from 2 with
// the key alias/payer named before as a former one: how a hot wallet's
// key is replaced, on a schedule or when it may have been exposed. The signer takes the
// new key when it starts again, which make vault-payer-rotate does at the end; until
// then it signs nothing.
//
// With no vault deployed yet, it only makes the key. Run again, it changes nothing that
// is in place. No key is ever printed, only addresses.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/WebOleg/settled/internal/artifact"
	"github.com/WebOleg/settled/internal/awscreds"
	"github.com/WebOleg/settled/internal/chaincall"
	"github.com/WebOleg/settled/internal/devkeys"
	"github.com/WebOleg/settled/internal/funding"
	"github.com/WebOleg/settled/internal/rolekeys"
	"github.com/WebOleg/settled/internal/signer"
	"github.com/WebOleg/settled/internal/sweep"
	"github.com/WebOleg/settled/internal/timelockop"
	"github.com/WebOleg/settled/internal/vaultconf"
)

const (
	mnemonicEnv = "SETTLED_DEVNET_MNEMONIC"
	// userIndex is the devnet user, who holds ETH on the L2 and pays the keys' gas.
	userIndex = 2
	// alias names the payer's key in the KMS; the signer may use no other.
	alias = "alias/payer"
	// retiring names the key a rotation replaced, for key rotation to move its funds.
	retiring = "alias/payer-retiring"
)

// copies are where the payer's key was written in a file for a container before it
// moved to the KMS: for the signer's, and before that the payer's. None may stay.
var copies = []string{".devnet/bridge/signing-key.txt", ".devnet/bridge/payer-key.txt"}

var (
	// gas is what the payer's account is topped up to: a few hundred withdrawals.
	gas = new(big.Int).Div(funding.Ether(1), big.NewInt(20)) // 0.05 ETH
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:18545", "sequencer execution JSON-RPC on the L2")
	outDir := flag.String("out", "contracts/out", "forge build output")
	endpoint := flag.String("kms", "http://127.0.0.1:14599", "the KMS's endpoint (kms-dev, published on this machine only)")
	region := flag.String("kms-region", "eu-central-1", "the KMS's region")
	creds := flag.String("kms-credentials", ".devnet/bridge/kms-admin.txt", "file holding the KMS admin's access key id and secret")
	rotationCreds := flag.String("kms-rotation-credentials", ".devnet/bridge/kms-rotation.txt", "file holding key rotation's access key id and secret for the KMS")
	rotate := flag.Bool("rotate", false, "make a new key in the KMS, move alias/payer to it and take the role from the one before")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := run(ctx, *rpcURL, *outDir, *endpoint, *region, *creds, *rotationCreds, *rotate); err != nil {
		fmt.Fprintf(os.Stderr, "vault-payer: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("vault-payer: ok")
}

func run(ctx context.Context, rpcURL, outDir, endpoint, region, creds, rotationCreds string, rotate bool) error {
	kmsClient := func(path string) (*kms.Client, error) {
		id, secret, err := awscreds.Read(path)
		if err != nil {
			return nil, fmt.Errorf("%w (make bridge-up makes them)", err)
		}
		return kms.New(kms.Options{Region: region, BaseEndpoint: aws.String(endpoint), Credentials: credentials.NewStaticCredentialsProvider(id, secret, "")}), nil
	}
	admin, err := kmsClient(creds)
	if err != nil {
		return err
	}
	rotation, err := kmsClient(rotationCreds)
	if err != nil {
		return err
	}
	keyID, payer, err := ensureKey(ctx, admin, rotate)
	if err != nil {
		return err
	}
	// No copy of a key in a file is left for a container.
	for _, path := range copies {
		if err := os.Remove(path); err == nil {
			fmt.Printf("key              removed %s: the signer's key is in the KMS\n", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	d, err := vaultconf.ReadDeployment(".")
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("vault            not deployed yet; make vault-deploy gives %s the operator role\n", payer)
		return nil
	}
	if err != nil {
		return err
	}
	chain, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	defer chain.Close()
	genesis, err := chain.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return fmt.Errorf("read the L2 genesis: %w", err)
	}
	if genesis.Hash() != d.L2Genesis {
		return errors.New("the deployment record belongs to another L2; run make vault-reset and make vault-deploy")
	}
	if d.NextTimelock != nil {
		return errors.New("a move to another timelock is half done; run make vault-timelock-move first")
	}
	user, err := devUser()
	if err != nil {
		return err
	}
	if _, err := rolekeys.Ensure(vaultconf.KeysDir, vaultconf.Roles); err != nil {
		return err
	}
	ops := &timelock{ctx: ctx, client: chain, outDir: outDir, d: d, user: user}

	// The new key first: withdrawals always have a payer.
	if err := ops.ensure(payer, true, "grantRole", "operator role to the payer"); err != nil {
		return err
	}
	if err := topUp(ctx, chain, user, payer, gas); err != nil {
		return err
	}

	// Then each key the payer held before has its role taken away.
	var former []common.Address
	if d.Payer != nil {
		former = append(former, d.Payer.Former...)
		former = append(former, d.Payer.Address)
	}
	file, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.FormerPayer)
	switch {
	case err == nil:
		former = append(former, crypto.PubkeyToAddress(file.PublicKey))
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	slices.SortFunc(former, func(a, b common.Address) int { return a.Cmp(b) })
	former = slices.DeleteFunc(slices.Compact(former), func(a common.Address) bool { return a == payer })
	for _, a := range former {
		if err := ops.ensure(a, false, "revokeRole", "operator role from the former payer"); err != nil {
			return err
		}
	}
	// Each former key that can still sign, now with no role, is emptied into the new
	// account: a key in a file, and the key a rotation replaced.
	if file != nil {
		if err := empty(ctx, chain, d.Vault, signer.NewFileKey(file), payer, "in a file"); err != nil {
			return err
		}
	}
	if old, err := signer.NewKMSKey(ctx, rotation, retiring); err == nil && slices.Contains(former, old.Address()) {
		if err := empty(ctx, chain, d.Vault, old, payer, "in the KMS, "+retiring); err != nil {
			return err
		}
	} else if err != nil && !isNotFound(err) {
		return err
	}

	d.Payer = &vaultconf.Payer{Address: payer, KMSKey: keyID, Former: former}
	if err := vaultconf.WriteDeployment(".", d); err != nil {
		return err
	}

	// The payer holds the operator role, and only that; a former key holds none.
	for _, r := range []struct {
		name string
		role common.Hash
	}{{"OPERATOR_ROLE", vaultconf.OperatorRole}, {"GUARDIAN_ROLE", vaultconf.GuardianRole}, {"DEFAULT_ADMIN_ROLE", vaultconf.AdminRole}} {
		for _, a := range append([]common.Address{payer}, former...) {
			got, err := hasRole(ctx, chain, d.Vault, r.role, a)
			if err != nil {
				return err
			}
			if want := a == payer && r.role == vaultconf.OperatorRole; got != want {
				return fmt.Errorf("%s: hasRole(%s) is %t, want %t", a, r.name, got, want)
			}
		}
	}
	balance, err := chain.BalanceAt(ctx, payer, nil)
	if err != nil {
		return err
	}
	fmt.Printf("payer            %s holds OPERATOR_ROLE and nothing else; %s ETH for gas; its key is in the KMS\n", payer, funding.FormatEther(balance))
	if len(former) > 0 {
		fmt.Printf("former           %d keys, none with a role on the vault\n", len(former))
	}
	return nil
}

// ensureKey finds the payer's key in the KMS by its alias, or makes it there and names
// it; with rotate, it makes a new one and moves the alias to it. The KMS may have just
// started: it is asked again until it answers.
func ensureKey(ctx context.Context, admin *kms.Client, rotate bool) (string, common.Address, error) {
	var described *kms.DescribeKeyOutput
	var err error
	for i := 0; ; i++ {
		described, err = admin.DescribeKey(ctx, &kms.DescribeKeyInput{KeyId: aws.String(alias)})
		var notFound *kmstypes.NotFoundException
		if err == nil || errors.As(err, &notFound) || i == 15 {
			break
		}
		select {
		case <-ctx.Done():
			return "", common.Address{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	var notFound *kmstypes.NotFoundException
	switch {
	case errors.As(err, &notFound):
		made, err := admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify})
		if err != nil {
			return "", common.Address{}, fmt.Errorf("make the payer's key in the KMS: %w", err)
		}
		if _, err := admin.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String(alias), TargetKeyId: made.KeyMetadata.KeyId}); err != nil {
			return "", common.Address{}, fmt.Errorf("name the payer's key %s in the KMS: %w", alias, err)
		}
		described = &kms.DescribeKeyOutput{KeyMetadata: made.KeyMetadata}
		fmt.Printf("key              made in the KMS, %s\n", alias)
	case err != nil:
		return "", common.Address{}, fmt.Errorf("find %s in the KMS (make bridge-up starts kms-dev): %w", alias, err)
	case rotate:
		// The key alias/payer names now is named alias/payer-retiring first, so it is
		// never without a name: key rotation empties it once it holds no role.
		old := described.KeyMetadata.KeyId
		if _, err := admin.UpdateAlias(ctx, &kms.UpdateAliasInput{AliasName: aws.String(retiring), TargetKeyId: old}); isNotFound(err) {
			_, err = admin.CreateAlias(ctx, &kms.CreateAliasInput{AliasName: aws.String(retiring), TargetKeyId: old})
			if err != nil {
				return "", common.Address{}, fmt.Errorf("name the old key %s: %w", retiring, err)
			}
		} else if err != nil {
			return "", common.Address{}, fmt.Errorf("name the old key %s: %w", retiring, err)
		}
		made, err := admin.CreateKey(ctx, &kms.CreateKeyInput{KeySpec: kmstypes.KeySpecEccSecgP256k1, KeyUsage: kmstypes.KeyUsageTypeSignVerify})
		if err != nil {
			return "", common.Address{}, fmt.Errorf("make the payer's new key in the KMS: %w", err)
		}
		// The new key is checked before the alias moves to it.
		if _, err := signer.NewKMSKey(ctx, admin, aws.ToString(made.KeyMetadata.KeyId)); err != nil {
			return "", common.Address{}, err
		}
		if _, err := admin.UpdateAlias(ctx, &kms.UpdateAliasInput{AliasName: aws.String(alias), TargetKeyId: made.KeyMetadata.KeyId}); err != nil {
			return "", common.Address{}, fmt.Errorf("move %s to the new key: %w", alias, err)
		}
		fmt.Printf("key              made a new one in the KMS; %s moved to it from %s\n", alias, aws.ToString(described.KeyMetadata.KeyId))
		described = &kms.DescribeKeyOutput{KeyMetadata: made.KeyMetadata}
	}
	// NewKMSKey checks the key is secp256k1 and for signing, and derives its account.
	k, err := signer.NewKMSKey(ctx, admin, aws.ToString(described.KeyMetadata.KeyId))
	if err != nil {
		return "", common.Address{}, err
	}
	fmt.Printf("key              %s in the KMS, account %s\n", alias, k.Address())
	return aws.ToString(described.KeyMetadata.KeyId), k.Address(), nil
}

// timelock runs grantRole and revokeRole on the vault through its timelock.
type timelock struct {
	ctx    context.Context
	client *ethclient.Client
	outDir string
	d      *vaultconf.Deployment
	user   *ecdsa.PrivateKey
	funded bool
}

// ensure makes account hold the operator role, or not, through the timelock, when it
// does not already.
func (t *timelock) ensure(account common.Address, hold bool, method, what string) error {
	holds, err := hasRole(t.ctx, t.client, t.d.Vault, vaultconf.OperatorRole, account)
	if err != nil || holds == hold {
		return err
	}
	tl, err := artifact.Load(t.outDir, "TimelockController.sol", "TimelockController")
	if err != nil {
		return err
	}
	vault, err := artifact.Load(t.outDir, "Vault.sol", "Vault")
	if err != nil {
		return err
	}
	proposer, err := rolekeys.Load(vaultconf.KeysDir, vaultconf.TimelockProposer)
	if err != nil {
		return err
	}
	if !t.funded {
		if err := topUp(t.ctx, t.client, t.user, crypto.PubkeyToAddress(proposer.PublicKey), new(big.Int).Div(funding.Ether(1), big.NewInt(50))); err != nil {
			return err
		}
		t.funded = true
	}
	data, err := vault.Call(method, vaultconf.OperatorRole, account)
	if err != nil {
		return err
	}
	op := timelockop.Op{Target: t.d.Vault, Data: data, Salt: crypto.Keccak256Hash([]byte("settled: " + what + " " + account.Hex()))}
	if _, _, err := timelockop.Run(t.ctx, t.client, proposer, tl, t.d.Timelock, op, big.NewInt(vaultconf.TimelockDelay), func(s string) {
		fmt.Printf("timelock         %s(OPERATOR_ROLE, %s): %s\n", method, account, s)
	}); err != nil {
		return fmt.Errorf("%s through the timelock: %w", what, err)
	}
	return nil
}

// empty moves the ETH of a former key to the payer's account, once the key holds no
// role on the vault: whoever signs with it then signs for nothing but its gas money.
func empty(ctx context.Context, client *ethclient.Client, vault common.Address, key sweep.Key, to common.Address, where string) error {
	from := key.Address()
	for _, role := range []common.Hash{vaultconf.OperatorRole, vaultconf.GuardianRole, vaultconf.AdminRole} {
		holds, err := hasRole(ctx, client, vault, role, from)
		if err != nil {
			return err
		}
		if holds {
			return fmt.Errorf("the former payer %s still holds a role on the vault; it is not emptied", from)
		}
	}
	r, err := sweep.Sweep(ctx, client, key, to)
	if err != nil {
		return fmt.Errorf("move the former payer's ETH to %s: %w", to, err)
	}
	if r.Moved.Sign() > 0 {
		fmt.Printf("former           %s (%s): %s ETH moved to the payer's account; %s wei left, the fee cap above the fee paid\n",
			from, where, funding.FormatEther(r.Moved), r.Left)
	}
	return nil
}

func isNotFound(err error) bool {
	var notFound *kmstypes.NotFoundException
	return errors.As(err, &notFound)
}

func hasRole(ctx context.Context, client *ethclient.Client, vault common.Address, role common.Hash, account common.Address) (bool, error) {
	words, err := chaincall.Words(ctx, client, vault, "hasRole(bytes32,address)", role, chaincall.AddressWord(account))
	if err != nil {
		return false, err
	}
	if len(words) != 1 {
		return false, fmt.Errorf("hasRole: %d words, want 1", len(words))
	}
	return words[0] == common.BigToHash(big.NewInt(1)), nil
}

func devUser() (*ecdsa.PrivateKey, error) {
	mnemonic := os.Getenv(mnemonicEnv)
	if mnemonic == "" {
		return nil, fmt.Errorf("%s is not set", mnemonicEnv)
	}
	user, err := devkeys.FromMnemonic(mnemonic, userIndex)
	if err != nil {
		return nil, fmt.Errorf("derive the devnet user key: %w", err)
	}
	return user, nil
}

func topUp(ctx context.Context, client *ethclient.Client, user *ecdsa.PrivateKey, account common.Address, balance *big.Int) error {
	if _, err := funding.TopUp(ctx, client, user, []funding.Target{{Name: account.Hex(), Address: account, Balance: balance}}); err != nil {
		return fmt.Errorf("top up %s from %s (make l2-deposit refills it): %w", account, crypto.PubkeyToAddress(user.PublicKey), err)
	}
	return nil
}
