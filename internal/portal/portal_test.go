package portal

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Selectors of the deployed functions, as published for OptimismPortal and
// L2ToL1MessagePasser.
func TestSelectors(t *testing.T) {
	tx := WithdrawalTx{Nonce: big.NewInt(0), Value: big.NewInt(0), GasLimit: big.NewInt(0), Data: []byte{}}
	cases := map[string]func() ([]byte, error){
		"e9e05c42": func() ([]byte, error) { return Deposit(common.Address{}, big.NewInt(1), 100_000) },
		"c2b3e5ac": func() ([]byte, error) { return InitiateWithdrawal(common.Address{}, big.NewInt(1), nil) },
		"4870496f": func() ([]byte, error) { return Prove(tx, big.NewInt(0), OutputRootProof{}, [][]byte{{1}}) },
		"8c3152e9": func() ([]byte, error) { return Finalize(tx) },
		"a14238e7": func() ([]byte, error) { return FinalizedWithdrawals(common.Hash{}) },
	}
	for want, pack := range cases {
		data, err := pack()
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if got := hex.EncodeToString(data[:4]); got != want {
			t.Errorf("selector %s, want %s", got, want)
		}
	}
}

// Hashes computed independently by encoding the six fields by hand.
func TestHash(t *testing.T) {
	sender := common.HexToAddress("0x1111111111111111111111111111111111111111")
	target := common.HexToAddress("0x2222222222222222222222222222222222222222")
	versioned := new(big.Int).Or(new(big.Int).Lsh(big.NewInt(1), 240), big.NewInt(7))
	cases := []struct {
		tx   WithdrawalTx
		want string
	}{
		{WithdrawalTx{versioned, sender, target, big.NewInt(1e17), big.NewInt(100_000), []byte{}},
			"0x34a85e81d465d19bddb715bda362c46f2f37bf40e6a37a53b6980eaa8b26d08f"},
		{WithdrawalTx{big.NewInt(0), sender, target, big.NewInt(1), big.NewInt(21_000), []byte{0xde, 0xad, 0xbe, 0xef}},
			"0x19b19e6d8e483147dcc7a5539acd559af40345a7b70ce48200b1264b4f71b539"},
	}
	for i, c := range cases {
		got, err := Hash(c.tx)
		if err != nil {
			t.Fatal(err)
		}
		if got != common.HexToHash(c.want) {
			t.Errorf("case %d: got %s, want %s", i, got, c.want)
		}
	}
}

func TestSentMessageSlot(t *testing.T) {
	hash := common.HexToHash("0x34a85e81d465d19bddb715bda362c46f2f37bf40e6a37a53b6980eaa8b26d08f")
	want := common.HexToHash("0x87a2be1f4b5f130872e4718f32b0a97ea50084b26a78be39abe3da594ed40e6b")
	if got := SentMessageSlot(hash); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// Selectors computed independently from the error names.
func TestErrorName(t *testing.T) {
	cases := map[string]string{
		"0x730a1074":         "OptimismPortal_AlreadyFinalized",
		"d9bc01be":           "OptimismPortal_ProofNotOldEnough",
		"0xCCA6AFDA":         "OptimismPortal_Unproven",
		"0x2e57ef3a00000000": "OptimismPortal_InvalidMerkleProof",
		"0x12345678":         "",
		"0x":                 "",
	}
	for data, want := range cases {
		if got := ErrorName(data); got != want {
			t.Errorf("ErrorName(%q) = %q, want %q", data, got, want)
		}
	}
}
