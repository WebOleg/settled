package bytecode

import (
	"bytes"
	"errors"
	"testing"
)

func TestCompareEqual(t *testing.T) {
	values, err := Compare([]byte{1, 2, 3}, []byte{1, 2, 3}, nil)
	if err != nil || len(values) != 0 {
		t.Fatalf("Compare = %v, %v", values, err)
	}
}

func TestCompareTakesImmutablesFromDeployed(t *testing.T) {
	built := []byte{0x60, 0, 0, 0, 0x56}
	deployed := []byte{0x60, 0xaa, 0xbb, 0, 0x56}
	values, err := Compare(built, deployed, []Range{{Start: 1, Length: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || !bytes.Equal(values[0], []byte{0xaa, 0xbb}) {
		t.Fatalf("values = %x", values)
	}
	// The returned value is a copy: changing deployed afterwards does not change it.
	deployed[1] = 0
	if values[0][0] != 0xaa {
		t.Fatal("value shares memory with deployed")
	}
}

func TestCompareFindsDifferenceOutsideImmutables(t *testing.T) {
	_, err := Compare([]byte{1, 0, 0, 4}, []byte{1, 9, 9, 5}, []Range{{Start: 1, Length: 2}})
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("err = %v, want ErrMismatch", err)
	}
}

func TestCompareRefusesBadInput(t *testing.T) {
	if _, err := Compare([]byte{1, 2}, []byte{1}, nil); !errors.Is(err, ErrMismatch) {
		t.Fatalf("length: err = %v", err)
	}
	if _, err := Compare(nil, nil, nil); err == nil {
		t.Fatal("empty build accepted")
	}
	if _, err := Compare([]byte{1, 2}, []byte{1, 2}, []Range{{Start: 1, Length: 2}}); err == nil || errors.Is(err, ErrMismatch) {
		t.Fatalf("range outside code: err = %v", err)
	}
}
