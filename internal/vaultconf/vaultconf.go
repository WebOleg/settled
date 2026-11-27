// Package vaultconf holds what the devnet deployment of the vault is: its keys, its
// parameters, and the record of a deployment. vault-deploy writes from it and
// vault-verify checks the chain against it, so both read the same numbers.
package vaultconf

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/WebOleg/settled/internal/rolekeys"
)

// Dir is where the deployment lives, KeysDir its keys, one file per role.
const (
	Dir            = ".devnet/vault"
	KeysDir        = ".devnet/vault/keys"
	DeploymentFile = "deployment.json"
)

// The keys of the vault. Each holds one role and nothing else; the deployer holds no
// role once the deployment is done.
const (
	Deployer         rolekeys.Role = "deployer"
	Operator         rolekeys.Role = "operator"
	Guardian         rolekeys.Role = "guardian"
	TimelockProposer rolekeys.Role = "timelock-proposer"
)

// Roles lists the keys of the vault.
var Roles = []rolekeys.Role{Deployer, Operator, Guardian, TimelockProposer}

// Devnet parameters. Production keeps the same structure with longer delays (days for
// the timelock and the admin handover, hours for the queue) and limits set from the
// expected daily flow.
const (
	// TimelockDelay is how long every admin change waits, in seconds.
	TimelockDelay = 60
	// AdminDelay is how long a new admin waits before it can accept the role.
	AdminDelay = 300
	// WithdrawalDelay is how long a large withdrawal waits in the queue, in L2 blocks
	// (the vault's clock): 2 minutes.
	WithdrawalDelay = 60
	// RefillPeriod is how many L2 blocks an empty rate limit takes to refill: an hour.
	RefillPeriod = 1800
)

// The vault's clock (ERC-6372) counts L2 blocks, and its block counts are durations
// only at the L2's block time, which vault-verify checks against the rollup config.
const (
	// ClockMode is what the vault's CLOCK_MODE() returns.
	ClockMode = "mode=blocknumber&from=default"
	// L2BlockTime is the seconds between L2 blocks.
	L2BlockTime = 2
)

// Native is the address the vault uses for ETH.
var Native = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")

// Ether returns n ETH in wei.
func Ether(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000_000_000_000_000))
}

// TokenConfig mirrors IVault.TokenConfig, field for field, for ABI encoding.
type TokenConfig struct {
	Allowed        bool
	Capacity       *big.Int
	RefillPeriod   uint64
	LargeThreshold *big.Int
}

// NativeConfig is the first configuration of ETH: up to 10 ETH at once per hour, and
// withdrawals of 5 ETH or more queued.
func NativeConfig() TokenConfig {
	return TokenConfig{Allowed: true, Capacity: Ether(10), RefillPeriod: RefillPeriod, LargeThreshold: Ether(5)}
}

// Validate applies the vault's own rule for a configuration, so a bad one is refused
// before it is proposed to the timelock.
func (c TokenConfig) Validate() error {
	if !c.Allowed || c.Capacity == nil || c.LargeThreshold == nil {
		return errors.New("vaultconf: an allowed token needs a capacity and a threshold")
	}
	if c.Capacity.Sign() <= 0 || c.RefillPeriod == 0 || c.LargeThreshold.Sign() <= 0 || c.LargeThreshold.Cmp(c.Capacity) > 0 {
		return fmt.Errorf("vaultconf: capacity %s, refill %d blocks, threshold %s: the threshold must be positive and at most the capacity", c.Capacity, c.RefillPeriod, c.LargeThreshold)
	}
	return nil
}

// Role hashes, as the contracts compute them.
var (
	OperatorRole  = crypto.Keccak256Hash([]byte("OPERATOR_ROLE"))
	GuardianRole  = crypto.Keccak256Hash([]byte("GUARDIAN_ROLE"))
	AdminRole     = common.Hash{}
	ProposerRole  = crypto.Keccak256Hash([]byte("PROPOSER_ROLE"))
	ExecutorRole  = crypto.Keccak256Hash([]byte("EXECUTOR_ROLE"))
	CancellerRole = crypto.Keccak256Hash([]byte("CANCELLER_ROLE"))
)

// Deployment is the record vault-deploy writes and vault-verify reads.
type Deployment struct {
	ChainID        uint64         `json:"chainId"`
	L2Genesis      common.Hash    `json:"l2Genesis"`
	Timelock       common.Address `json:"timelock"`
	Implementation common.Address `json:"implementation"`
	Vault          common.Address `json:"vault"`
	Block          uint64         `json:"block"`
}

// ReadDeployment reads Dir/deployment.json from root. It wraps fs.ErrNotExist when there
// is none.
func ReadDeployment(root string) (*Deployment, error) {
	raw, err := os.ReadFile(filepath.Join(root, Dir, DeploymentFile))
	if err != nil {
		return nil, fmt.Errorf("vaultconf: %w", err)
	}
	var d Deployment
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("vaultconf: parse %s: %w", DeploymentFile, err)
	}
	return &d, nil
}

// WriteDeployment writes Dir/deployment.json under root through a temporary file and a
// rename, so a reader never sees half a record.
func WriteDeployment(root string, d *Deployment) error {
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("vaultconf: encode deployment: %w", err)
	}
	path := filepath.Join(root, Dir, DeploymentFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("vaultconf: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("vaultconf: rename %s: %w", tmp, err)
	}
	return nil
}
