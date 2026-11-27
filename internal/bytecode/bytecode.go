// Package bytecode compares the code of a deployed contract with the code a local build
// produced: byte for byte, except the ranges the compiler marks as immutables, which the
// constructor writes into the code at deployment. Their values are returned so the
// caller can check them as well.
package bytecode

import (
	"bytes"
	"errors"
	"fmt"
)

// Range is one immutable slot in runtime code: Length bytes from Start.
type Range struct {
	Start  int `json:"start"`
	Length int `json:"length"`
}

// ErrMismatch is returned when deployed code differs from the build.
var ErrMismatch = errors.New("bytecode: deployed code differs from the build")

// Compare checks deployed against built, taking each immutable range from deployed, and
// returns the immutable values in the order of the ranges.
func Compare(built, deployed []byte, immutables []Range) ([][]byte, error) {
	if len(built) == 0 {
		return nil, errors.New("bytecode: the build has no runtime code")
	}
	if len(deployed) != len(built) {
		return nil, fmt.Errorf("%w: %d bytes deployed, %d built", ErrMismatch, len(deployed), len(built))
	}
	masked := bytes.Clone(built)
	values := make([][]byte, 0, len(immutables))
	for _, r := range immutables {
		if r.Start < 0 || r.Length <= 0 || r.Start+r.Length > len(built) {
			return nil, fmt.Errorf("bytecode: immutable range %d+%d outside %d bytes of code", r.Start, r.Length, len(built))
		}
		copy(masked[r.Start:r.Start+r.Length], deployed[r.Start:r.Start+r.Length])
		values = append(values, bytes.Clone(deployed[r.Start:r.Start+r.Length]))
	}
	for i := range masked {
		if masked[i] != deployed[i] {
			return nil, fmt.Errorf("%w: first difference at byte %d", ErrMismatch, i)
		}
	}
	return values, nil
}
