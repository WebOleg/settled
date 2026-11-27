package main

import (
	"math/big"
	"testing"
)

func TestEther_writesWeiExactly(t *testing.T) {
	for wei, want := range map[string]string{"0": "0", "1": "0.000000000000000001", "3000000000000000000": "3", "500000000000000000": "0.5"} {
		n, _ := new(big.Int).SetString(wei, 10)
		if got := ether(n); got != want {
			t.Errorf("ether(%s) = %s, want %s", wei, got, want)
		}
	}
}
