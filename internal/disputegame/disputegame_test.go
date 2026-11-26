package disputegame

import (
	"errors"
	"testing"
)

func rollup() Rollup {
	var r Rollup
	r.BlockTime = 2
	r.L2ChainID = 420420
	r.Genesis.L2Time = 1795700188
	return r
}

func TestBlockAt(t *testing.T) {
	r := rollup()
	cases := map[uint64]uint64{1795700188: 0, 1795700190: 1, 1795700188 + 2*141: 141}
	for ts, want := range cases {
		got, err := r.BlockAt(ts)
		if err != nil || got != want {
			t.Errorf("BlockAt(%d) = %d, %v; want %d", ts, got, err, want)
		}
	}
}

func TestBlockAtRejectsOtherTimes(t *testing.T) {
	r := rollup()
	for _, ts := range []uint64{1795700187, 1795700189} {
		if _, err := r.BlockAt(ts); !errors.Is(err, ErrNotABlockTime) {
			t.Errorf("BlockAt(%d): got %v, want ErrNotABlockTime", ts, err)
		}
	}
	if _, err := (Rollup{}).BlockAt(0); err == nil {
		t.Error("a rollup config without a block time was accepted")
	}
}

func TestStatusName(t *testing.T) {
	if StatusName(DefenderWins) != "DEFENDER_WINS" || StatusName(9) != "status 9" {
		t.Fatal("unexpected status names")
	}
}
