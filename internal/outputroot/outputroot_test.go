package outputroot

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// One block of the devnet L2 at timestamp 1795700386, as returned by op-node's
// superroot_atTimestamp. The output root starts with the 45 hex digits op-node printed;
// the rest and the super roots were computed independently from the encoding above.
var (
	stateRoot  = common.HexToHash("0xeb431b5242c38943dfaf53eb094e47be54c5fec60de191a3387f186b1c171ea2")
	passerRoot = common.HexToHash("0x8ed4baae3a927be3dea54996b4d5899f8c01e7594bf50b17dc1e741388ce3d12")
	blockHash  = common.HexToHash("0xcccb045164b5cb68e51d8bc1af14044a911cb21d4acedef09a73f570ef0ee6bb")
	outputRoot = common.HexToHash("0xa1f1a5948622fe238fb8489fbdde9363113db49d891953dc6d22b70bfef6c10b")
)

const timestamp = 1795700386

func TestV0(t *testing.T) {
	if got := V0(stateRoot, passerRoot, blockHash); got != outputRoot {
		t.Fatalf("got %s, want %s", got, outputRoot)
	}
}

func TestV0DependsOnEachField(t *testing.T) {
	other := common.HexToHash("0x01")
	for name, got := range map[string]common.Hash{
		"state root":    V0(other, passerRoot, blockHash),
		"passer root":   V0(stateRoot, other, blockHash),
		"block hash":    V0(stateRoot, passerRoot, other),
		"swapped roots": V0(passerRoot, stateRoot, blockHash),
	} {
		if got == outputRoot {
			t.Errorf("changing the %s did not change the output root", name)
		}
	}
}

func TestSuperV1OneChain(t *testing.T) {
	want := common.HexToHash("0x79b4161d4057b932132f7737449577b49468f361a4439b7836172f939bdb1746")
	got := SuperV1(timestamp, []ChainOutput{{ChainID: 420420, Output: outputRoot}})
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSuperV1SortsChains(t *testing.T) {
	want := common.HexToHash("0x09c4bd9eaf34f3d239b8af247b1608fee5ff4c6a652dce6612d4748eff78b11b")
	ten := ChainOutput{ChainID: 10, Output: common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")}
	ours := ChainOutput{ChainID: 420420, Output: outputRoot}
	for _, order := range [][]ChainOutput{{ten, ours}, {ours, ten}} {
		if got := SuperV1(timestamp, order); got != want {
			t.Errorf("order %v: got %s, want %s", []uint64{order[0].ChainID, order[1].ChainID}, got, want)
		}
	}
}

func TestSuperV1DependsOnTimestamp(t *testing.T) {
	chains := []ChainOutput{{ChainID: 420420, Output: outputRoot}}
	if SuperV1(timestamp, chains) == SuperV1(timestamp+2, chains) {
		t.Fatal("the timestamp does not change the super root")
	}
}
