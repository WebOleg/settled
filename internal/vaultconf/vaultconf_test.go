package vaultconf

import (
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/WebOleg/settled/internal/storagelayout"
)

func TestNativeConfigIsValid(t *testing.T) {
	if err := NativeConfig().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusesWhatTheVaultRefuses(t *testing.T) {
	bad := []TokenConfig{
		{Allowed: false, Capacity: Ether(1), RefillPeriod: 1, LargeThreshold: Ether(1)},
		{Allowed: true, Capacity: Ether(1), RefillPeriod: 1, LargeThreshold: Ether(2)},
		{Allowed: true, Capacity: Ether(1), RefillPeriod: 0, LargeThreshold: Ether(1)},
		{Allowed: true, Capacity: big.NewInt(0), RefillPeriod: 1, LargeThreshold: big.NewInt(0)},
		{Allowed: true, Capacity: Ether(1), RefillPeriod: 1, LargeThreshold: big.NewInt(0)},
		{Allowed: true},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("config %d accepted: %+v", i, c)
		}
	}
}

func TestDelaysAreWithinTheVaultBounds(t *testing.T) {
	// The vault takes a queue delay from 30 to 1,296,000 blocks (MIN_WITHDRAWAL_DELAY and
	// MAX_WITHDRAWAL_DELAY): 1 minute to 30 days at the L2's block time.
	if WithdrawalDelay < 30 || WithdrawalDelay > 1_296_000 {
		t.Fatalf("withdrawal delay %d outside the vault's bounds", WithdrawalDelay)
	}
	if WithdrawalDelay*L2BlockTime != 120 || RefillPeriod*L2BlockTime != 3600 {
		t.Fatalf("queue %d blocks, refill %d blocks: want 2 minutes and an hour", WithdrawalDelay, RefillPeriod)
	}
	if TimelockDelay <= 0 || AdminDelay <= 0 {
		t.Fatal("delays must be positive")
	}
}

func TestRoleHashes(t *testing.T) {
	want := common.HexToHash("0x97667070c54ef182b0f5858b034beac1b6f3089aa2d3188bb1e8929f4fa9b929")
	if OperatorRole != want {
		t.Fatalf("OPERATOR_ROLE = %s", OperatorRole)
	}
}

func TestDeploymentRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadDeployment(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: err = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Deployment{
		ChainID: 420420, Vault: common.HexToAddress("0x01"), Block: 7,
		StorageLayout: []storagelayout.Entry{{Label: "_n", Slot: "5", Offset: 0, Bytes: 32, Type: "uint256:32"}},
	}
	if err := WriteDeployment(root, d); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDeployment(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("got %+v, want %+v", got, d)
	}
}
