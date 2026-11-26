package l2intent

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/WebOleg/settled/internal/rolekeys"
)

func roles() map[rolekeys.Role]common.Address {
	m := make(map[rolekeys.Role]common.Address, len(rolekeys.All))
	for i, r := range rolekeys.All {
		m[r] = common.BigToAddress(big.NewInt(int64(0x1000 + i)))
	}
	return m
}

func params() Params {
	return Params{L1ChainID: 3151908, L2ChainID: 420420, OpDeployerVersion: "v0.9.0-test", Roles: roles()}
}

// field returns the value of `key = "..."` inside the given TOML section.
func field(t *testing.T, doc, section, key string) string {
	t.Helper()
	in := section == ""
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			in = trimmed == section
			continue
		}
		if in && strings.HasPrefix(trimmed, key+" = ") {
			return strings.Trim(strings.TrimPrefix(trimmed, key+" = "), `"`)
		}
	}
	t.Fatalf("%s %s not found", section, key)
	return ""
}

func TestRenderPutsEachRoleInItsField(t *testing.T) {
	p := params()
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	want := []struct {
		section, key string
		role         rolekeys.Role
	}{
		{"[superchainRoles]", "SuperchainProxyAdminOwner", rolekeys.SuperchainAdmin},
		{"[superchainRoles]", "SuperchainGuardian", rolekeys.Guardian},
		{"[superchainRoles]", "Challenger", rolekeys.Challenger},
		{"[[chains]]", "baseFeeVaultRecipient", rolekeys.FeeRecipient},
		{"[[chains]]", "l1FeeVaultRecipient", rolekeys.FeeRecipient},
		{"[[chains]]", "sequencerFeeVaultRecipient", rolekeys.FeeRecipient},
		{"[[chains]]", "operatorFeeVaultRecipient", rolekeys.FeeRecipient},
		{"[chains.roles]", "l1ProxyAdminOwner", rolekeys.ChainAdmin},
		{"[chains.roles]", "l2ProxyAdminOwner", rolekeys.ChainAdmin},
		{"[chains.roles]", "systemConfigOwner", rolekeys.SystemConfigOwner},
		{"[chains.roles]", "unsafeBlockSigner", rolekeys.Sequencer},
		{"[chains.roles]", "batcher", rolekeys.Batcher},
		{"[chains.roles]", "proposer", rolekeys.Proposer},
		{"[chains.roles]", "challenger", rolekeys.Challenger},
	}
	for _, w := range want {
		if got := field(t, doc, w.section, w.key); got != p.Roles[w.role].Hex() {
			t.Errorf("%s %s = %s, want %s (%s)", w.section, w.key, got, p.Roles[w.role].Hex(), w.role)
		}
	}
}

func TestRenderHasNoZeroAddressAndNoDeployer(t *testing.T) {
	p := params()
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	// Quoted, so the zero-padded chain id does not count as a zero address.
	if strings.Contains(string(out), `"`+common.Address{}.Hex()+`"`) {
		t.Error("intent contains the zero address")
	}
	if strings.Contains(string(out), p.Roles[rolekeys.Deployer].Hex()) {
		t.Error("intent gives a role to the deployer")
	}
}

func TestRenderChainParameters(t *testing.T) {
	out, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	checks := map[string]string{
		"id":                       "0x0000000000000000000000000000000000000000000000000000000000066a44",
		"eip1559Denominator":       "50",
		"eip1559DenominatorCanyon": "250",
		"eip1559Elasticity":        "6",
		"gasLimit":                 "60000000",
		"daFootprintGasScalar":     "400",
		"operatorFeeScalar":        "0",
		"operatorFeeConstant":      "0",
	}
	for key, want := range checks {
		if got := field(t, doc, "[[chains]]", key); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if got := field(t, doc, "", "l1ChainID"); got != "3151908" {
		t.Errorf("l1ChainID = %s, want 3151908", got)
	}
	if got := field(t, doc, "", "opDeployerVersion"); got != "v0.9.0-test" {
		t.Errorf("opDeployerVersion = %s", got)
	}
}

func TestRenderRejectsMissingRole(t *testing.T) {
	for _, r := range needed {
		p := params()
		delete(p.Roles, r)
		if _, err := Render(p); !errors.Is(err, ErrMissingRole) {
			t.Errorf("without %s: got %v, want ErrMissingRole", r, err)
		}
	}
}

func TestRenderRejectsDeployerAsRole(t *testing.T) {
	p := params()
	p.Roles[rolekeys.Batcher] = p.Roles[rolekeys.Deployer]
	if _, err := Render(p); err == nil {
		t.Fatal("deployer address as batcher: want an error, got none")
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	cases := map[string]func(*Params){
		"empty version": func(p *Params) { p.OpDeployerVersion = "" },
		"no l1 chain":   func(p *Params) { p.L1ChainID = 0 },
		"no l2 chain":   func(p *Params) { p.L2ChainID = 0 },
	}
	for name, mutate := range cases {
		p := params()
		mutate(&p)
		if _, err := Render(p); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

func TestRenderDisputeTimings(t *testing.T) {
	out, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	checks := map[string]string{
		"proofMaturityDelaySeconds":               "60",
		"disputeGameFinalityDelaySeconds":         "30",
		"faultGameMaxClockDuration":               "120",
		"faultGameClockExtension":                 "30",
		"preimageOracleChallengePeriod":           "30",
		"faultGameWithdrawalDelay":                "60",
		"dangerouslyAllowCustomDisputeParameters": "true",
	}
	for key, want := range checks {
		if got := field(t, doc, "[globalDeployOverrides]", key); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}

// The dispute game contracts refuse timings that break these relations, so a wrong
// value would only show up as a failed deployment.
func TestDisputeTimingsFitTheGameClock(t *testing.T) {
	if 2*FaultGameClockExtension > FaultGameMaxClockDuration {
		t.Errorf("2 x clock extension %d > max clock duration %d", 2*FaultGameClockExtension, FaultGameMaxClockDuration)
	}
	if FaultGameClockExtension+PreimageOracleChallengePeriod > FaultGameMaxClockDuration {
		t.Errorf("clock extension + preimage challenge period %d > max clock duration %d",
			FaultGameClockExtension+PreimageOracleChallengePeriod, FaultGameMaxClockDuration)
	}
}
