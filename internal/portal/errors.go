package portal

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

// portalErrors are the custom errors of OptimismPortal2 (op-contracts v9) that a call
// made by these tools can meet. None takes arguments.
var portalErrors = []string{
	"OptimismPortal_AlreadyFinalized",
	"OptimismPortal_BadTarget",
	"OptimismPortal_CallPaused",
	"OptimismPortal_CalldataTooLarge",
	"OptimismPortal_GasEstimation",
	"OptimismPortal_GasLimitTooLow",
	"OptimismPortal_ImproperDisputeGame",
	"OptimismPortal_InvalidDisputeGame",
	"OptimismPortal_InvalidMerkleProof",
	"OptimismPortal_InvalidOutputRootProof",
	"OptimismPortal_InvalidProofTimestamp",
	"OptimismPortal_InvalidRootClaim",
	"OptimismPortal_NoReentrancy",
	"OptimismPortal_ProofNotOldEnough",
	"OptimismPortal_Unproven",
	"OptimismPortal_DisputeGameNotInvalidated",
}

var errorBySelector = func() map[string]string {
	m := make(map[string]string, len(portalErrors))
	for _, name := range portalErrors {
		m[hex.EncodeToString(crypto.Keccak256([]byte(name + "()"))[:4])] = name
	}
	return m
}()

// ErrorName returns the name of the portal's custom error in revert data (hex, with or
// without 0x), or "" when the data is not one of them.
func ErrorName(revertData string) string {
	data := strings.TrimPrefix(revertData, "0x")
	if len(data) < 8 {
		return ""
	}
	return errorBySelector[strings.ToLower(data[:8])]
}

// RevertReason names the portal error behind a failed call or estimate, falling back to
// the error text when the node returned no revert data or an unknown selector.
func RevertReason(err error) string {
	var de rpc.DataError
	if errors.As(err, &de) {
		if data, ok := de.ErrorData().(string); ok {
			if name := ErrorName(data); name != "" {
				return name
			}
		}
	}
	return err.Error()
}
