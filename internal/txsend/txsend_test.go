package txsend

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestWithMargin(t *testing.T) {
	cases := map[uint64]uint64{0: 0, 100: 120, 21_000: 25_200, 99: 118}
	for in, want := range cases {
		if got := WithMargin(in); got != want {
			t.Errorf("WithMargin(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestExecutionCost(t *testing.T) {
	r := &types.Receipt{GasUsed: 21_000, EffectiveGasPrice: big.NewInt(7)}
	if got := ExecutionCost(r); got.Cmp(big.NewInt(147_000)) != 0 {
		t.Fatalf("got %s, want 147000", got)
	}
	if got := ExecutionCost(&types.Receipt{GasUsed: 1}); got.Sign() != 0 {
		t.Fatalf("no gas price: got %s, want 0", got)
	}
}
