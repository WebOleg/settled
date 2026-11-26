// Package l2intent renders the op-deployer intent file for the devnet L2. It fills every
// role with its own operator address and every chain parameter with the OP Stack
// standard value, so no zero address and no zero parameter reaches the deployment.
package l2intent

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"github.com/ethereum/go-ethereum/common"

	"github.com/WebOleg/settled/internal/rolekeys"
)

// Chain parameters. Sources are listed in docs/design/devnet.md.
const (
	// EIP-1559 values of the op-deployer standard configuration.
	EIP1559Denominator       = 50
	EIP1559DenominatorCanyon = 250
	EIP1559Elasticity        = 6
	// The op-deployer default; the standard configuration allows up to 200M.
	GasLimit = 60_000_000
	// Jovian treats 0 as 400. Written out so the file says what the chain uses.
	DAFootprintGasScalar = 400
)

// Params are the inputs of one intent file.
type Params struct {
	L1ChainID         uint64
	L2ChainID         uint64
	OpDeployerVersion string
	Roles             map[rolekeys.Role]common.Address
}

// ErrMissingRole is returned when a role needed by the intent has no address.
var ErrMissingRole = errors.New("l2intent: role has no address")

// needed lists the roles the intent refers to. The deployer signs the deployment and
// holds no role in it.
var needed = []rolekeys.Role{
	rolekeys.SuperchainAdmin, rolekeys.Guardian, rolekeys.ChainAdmin,
	rolekeys.SystemConfigOwner, rolekeys.Sequencer, rolekeys.Batcher,
	rolekeys.Proposer, rolekeys.Challenger, rolekeys.FeeRecipient,
}

// Render returns the intent file.
func Render(p Params) ([]byte, error) {
	if p.OpDeployerVersion == "" {
		return nil, errors.New("l2intent: op-deployer version is empty")
	}
	if p.L1ChainID == 0 || p.L2ChainID == 0 {
		return nil, errors.New("l2intent: chain ids must be set")
	}
	addr := make(map[string]string, len(needed))
	for _, r := range needed {
		a, ok := p.Roles[r]
		if !ok || a == (common.Address{}) {
			return nil, fmt.Errorf("%w: %s", ErrMissingRole, r)
		}
		addr[string(r)] = a.Hex()
	}
	if deployer, ok := p.Roles[rolekeys.Deployer]; ok {
		for _, r := range needed {
			if p.Roles[r] == deployer {
				return nil, fmt.Errorf("l2intent: %s must not be the deployer address", r)
			}
		}
	}

	var buf bytes.Buffer
	err := intentTemplate.Execute(&buf, map[string]any{
		"Version":     p.OpDeployerVersion,
		"L1ChainID":   p.L1ChainID,
		"L2ChainID":   fmt.Sprintf("0x%064x", p.L2ChainID),
		"A":           addr,
		"Denominator": EIP1559Denominator,
		"Canyon":      EIP1559DenominatorCanyon,
		"Elasticity":  EIP1559Elasticity,
		"GasLimit":    GasLimit,
		"DAScalar":    DAFootprintGasScalar,
	})
	if err != nil {
		return nil, fmt.Errorf("l2intent: render: %w", err)
	}
	return buf.Bytes(), nil
}

// The layout follows the file that `op-deployer init --intent-type custom` writes for
// op-deployer v0.9.0, with every value filled in.
var intentTemplate = template.Must(template.New("intent").Parse(`configType = "custom"
opDeployerVersion = "{{.Version}}"
l1ChainID = {{.L1ChainID}}
fundDevAccounts = false
l1ContractsLocator = "embedded"
l2ContractsLocator = "embedded"
useInterop = false

[superchainRoles]
  SuperchainProxyAdminOwner = "{{index .A "superchain-admin"}}"
  SuperchainGuardian = "{{index .A "guardian"}}"
  Challenger = "{{index .A "challenger"}}"

[[chains]]
  id = "{{.L2ChainID}}"
  baseFeeVaultRecipient = "{{index .A "fee-recipient"}}"
  l1FeeVaultRecipient = "{{index .A "fee-recipient"}}"
  sequencerFeeVaultRecipient = "{{index .A "fee-recipient"}}"
  operatorFeeVaultRecipient = "{{index .A "fee-recipient"}}"
  eip1559DenominatorCanyon = {{.Canyon}}
  eip1559Denominator = {{.Denominator}}
  eip1559Elasticity = {{.Elasticity}}
  gasLimit = {{.GasLimit}}
  operatorFeeScalar = 0
  operatorFeeConstant = 0
  minBaseFee = 0
  daFootprintGasScalar = {{.DAScalar}}
  [chains.roles]
    l1ProxyAdminOwner = "{{index .A "chain-admin"}}"
    l2ProxyAdminOwner = "{{index .A "chain-admin"}}"
    systemConfigOwner = "{{index .A "system-config-owner"}}"
    unsafeBlockSigner = "{{index .A "sequencer"}}"
    batcher = "{{index .A "batcher"}}"
    proposer = "{{index .A "proposer"}}"
    challenger = "{{index .A "challenger"}}"
`))
