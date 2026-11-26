package chaincall

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

func TestSelectorKnownValues(t *testing.T) {
	// Published selectors: Ownable.owner() and ERC20.transfer(address,uint256).
	cases := map[string]string{
		"owner()":                   "8da5cb5b",
		"transfer(address,uint256)": "a9059cbb",
	}
	for sig, want := range cases {
		if got := hex.EncodeToString(Selector(sig)); got != want {
			t.Errorf("Selector(%q) = %s, want %s", sig, got, want)
		}
	}
}

type fakeCaller struct {
	out []byte
	err error
	got ethereum.CallMsg
}

func (f *fakeCaller) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	f.got = msg
	return f.out, f.err
}

func TestWordSendsSelectorToContract(t *testing.T) {
	contract := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	f := &fakeCaller{out: make([]byte, 32)}
	if _, err := Word(context.Background(), f, contract, "owner()"); err != nil {
		t.Fatal(err)
	}
	if f.got.To == nil || *f.got.To != contract {
		t.Fatalf("call went to %v, want %s", f.got.To, contract)
	}
	if hex.EncodeToString(f.got.Data) != "8da5cb5b" {
		t.Fatalf("call data %x, want the owner() selector", f.got.Data)
	}
}

func TestWordRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 31, 64} {
		f := &fakeCaller{out: make([]byte, n)}
		if _, err := Word(context.Background(), f, common.Address{}, "owner()"); !errors.Is(err, ErrNotAWord) {
			t.Errorf("%d bytes: got %v, want ErrNotAWord", n, err)
		}
	}
}

func TestWordPassesCallErrors(t *testing.T) {
	boom := errors.New("execution reverted")
	if _, err := Word(context.Background(), &fakeCaller{err: boom}, common.Address{}, "owner()"); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the call error", err)
	}
}

func TestAddressRoundTrip(t *testing.T) {
	a := common.HexToAddress("0x9Bce86a5B626355c11947ceF3956621046602cd6")
	got, err := AddressOf(AddressWord(a))
	if err != nil {
		t.Fatal(err)
	}
	if got != a {
		t.Fatalf("got %s, want %s", got, a)
	}
}

func TestAddressOfRejectsHighBits(t *testing.T) {
	w := AddressWord(common.HexToAddress("0x01"))
	w[0] = 0x01
	if _, err := AddressOf(w); !errors.Is(err, ErrNotAnAddress) {
		t.Fatalf("got %v, want ErrNotAnAddress", err)
	}
}

func TestUintOf(t *testing.T) {
	w := common.BigToHash(big.NewInt(60_000_000))
	if UintOf(w).Cmp(big.NewInt(60_000_000)) != 0 {
		t.Fatalf("got %s", UintOf(w))
	}
}

func TestWordsSendsArgumentsAndSplitsTheReturn(t *testing.T) {
	out := make([]byte, 96)
	out[31], out[63], out[95] = 5, 7, 9
	f := &fakeCaller{out: out}
	words, err := Words(context.Background(), f, common.Address{}, "gameAtIndex(uint256)", UintWord(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 3 || words[0][31] != 5 || words[1][31] != 7 || words[2][31] != 9 {
		t.Fatalf("got %v", words)
	}
	if len(f.got.Data) != 36 || f.got.Data[35] != 3 {
		t.Fatalf("call data %x, want selector and the index word", f.got.Data)
	}
	if hex.EncodeToString(f.got.Data[:4]) != hex.EncodeToString(Selector("gameAtIndex(uint256)")) {
		t.Fatalf("call data %x does not start with the selector", f.got.Data)
	}
}

func TestWordsRejectsPartialWords(t *testing.T) {
	for _, n := range []int{0, 33, 65} {
		f := &fakeCaller{out: make([]byte, n)}
		if _, err := Words(context.Background(), f, common.Address{}, "x()"); !errors.Is(err, ErrBadReturn) {
			t.Errorf("%d bytes: got %v, want ErrBadReturn", n, err)
		}
	}
}
