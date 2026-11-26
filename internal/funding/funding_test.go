package funding

import (
	"math/big"
	"testing"
)

func TestShortfall(t *testing.T) {
	cases := []struct {
		name            string
		balance, target *big.Int
		want            *big.Int
	}{
		{"empty account", big.NewInt(0), Ether(20), Ether(20)},
		{"partly funded", Ether(5), Ether(20), Ether(15)},
		{"exactly funded", Ether(20), Ether(20), big.NewInt(0)},
		{"over funded", Ether(25), Ether(20), big.NewInt(0)},
		{"zero target", Ether(1), big.NewInt(0), big.NewInt(0)},
	}
	for _, c := range cases {
		got, err := Shortfall(c.balance, c.target)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Cmp(c.want) != 0 {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestShortfallRejectsBadInput(t *testing.T) {
	bad := [][2]*big.Int{
		{nil, Ether(1)},
		{Ether(1), nil},
		{big.NewInt(-1), Ether(1)},
		{Ether(1), big.NewInt(-1)},
	}
	for i, b := range bad {
		if _, err := Shortfall(b[0], b[1]); err == nil {
			t.Errorf("case %d: want an error, got none", i)
		}
	}
}

func TestShortfallDoesNotAliasInputs(t *testing.T) {
	balance, target := Ether(1), Ether(3)
	got, err := Shortfall(balance, target)
	if err != nil {
		t.Fatal(err)
	}
	got.SetInt64(0)
	if balance.Cmp(Ether(1)) != 0 || target.Cmp(Ether(3)) != 0 {
		t.Fatal("changing the result changed an input")
	}
}

func TestEther(t *testing.T) {
	want, _ := new(big.Int).SetString("20000000000000000000", 10)
	if Ether(20).Cmp(want) != 0 {
		t.Fatalf("Ether(20) = %s, want %s", Ether(20), want)
	}
}

func TestFormatEther(t *testing.T) {
	cases := []struct {
		wei  *big.Int
		want string
	}{
		{big.NewInt(0), "0.0000"},
		{Ether(20), "20.0000"},
		{new(big.Int).Add(Ether(1), new(big.Int).Div(Ether(1), big.NewInt(2))), "1.5000"},
		{big.NewInt(99_999_999_999_999), "0.0000"}, // below 0.0001 ETH truncates to zero
		{Ether(-2), "-2.0000"},
	}
	for _, c := range cases {
		if got := FormatEther(c.wei); got != c.want {
			t.Errorf("FormatEther(%s) = %s, want %s", c.wei, got, c.want)
		}
	}
}
