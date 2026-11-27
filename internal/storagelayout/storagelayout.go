// Package storagelayout reads the storage layout solc reports for a contract and decides
// whether a new version of an upgradeable contract keeps the state of the old one where
// it is. A proxy keeps the state and runs the new code on it, so every variable of the
// old version must be at the same slot and offset, with the same name and the same type,
// in the new one, and no new variable may share their bytes. Types are compared by what
// they are (encoding, size, members, keys, values), not by the AST ids in their names,
// which change from build to build.
package storagelayout

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Layout is solc's storageLayout output, as forge writes it into an artifact.
type Layout struct {
	Storage []variable      `json:"storage"`
	Types   map[string]kind `json:"types"`
}

type variable struct {
	Label  string `json:"label"`
	Offset int    `json:"offset"`
	Slot   string `json:"slot"`
	Type   string `json:"type"`
}

type kind struct {
	Encoding      string     `json:"encoding"`
	Label         string     `json:"label"`
	NumberOfBytes string     `json:"numberOfBytes"`
	Key           string     `json:"key"`
	Value         string     `json:"value"`
	Base          string     `json:"base"`
	Members       []variable `json:"members"`
}

// Entry is one variable of a layout, with its type written out in full.
type Entry struct {
	Label  string `json:"label"`
	Slot   string `json:"slot"`
	Offset int    `json:"offset"`
	Bytes  int    `json:"bytes"`
	Type   string `json:"type"`
}

// ErrIncompatible is wrapped by every reason Compatible gives.
var ErrIncompatible = errors.New("storagelayout: incompatible")

// Parse reads a storageLayout object and returns its variables in slot order.
func Parse(raw []byte) ([]Entry, error) {
	var l Layout
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("storagelayout: parse: %w", err)
	}
	if l.Storage == nil {
		return nil, errors.New("storagelayout: no storage section")
	}
	entries := make([]Entry, 0, len(l.Storage))
	for _, v := range l.Storage {
		t, err := l.describe(v.Type, 0)
		if err != nil {
			return nil, err
		}
		size, err := l.size(v.Type)
		if err != nil {
			return nil, err
		}
		if _, ok := new(big.Int).SetString(v.Slot, 10); !ok {
			return nil, fmt.Errorf("storagelayout: %s: slot %q is not a number", v.Label, v.Slot)
		}
		entries = append(entries, Entry{Label: v.Label, Slot: v.Slot, Offset: v.Offset, Bytes: size, Type: t})
	}
	sort.SliceStable(entries, func(i, j int) bool { return less(entries[i], entries[j]) })
	return entries, nil
}

// maxDepth bounds the type description: solc's types are finite, so a deeper one is a
// malformed input.
const maxDepth = 32

// describe writes a type out from its parts.
func (l *Layout) describe(id string, depth int) (string, error) {
	if depth > maxDepth {
		return "", fmt.Errorf("storagelayout: type %s nests too deep", id)
	}
	t, ok := l.Types[id]
	if !ok {
		return "", fmt.Errorf("storagelayout: unknown type %s", id)
	}
	switch {
	case t.Encoding == "mapping":
		k, err := l.describe(t.Key, depth+1)
		if err != nil {
			return "", err
		}
		v, err := l.describe(t.Value, depth+1)
		if err != nil {
			return "", err
		}
		return "mapping(" + k + "=>" + v + ")", nil
	case t.Base != "":
		b, err := l.describe(t.Base, depth+1)
		if err != nil {
			return "", err
		}
		return t.Encoding + ":" + t.NumberOfBytes + "[" + b + "]", nil
	case t.Members != nil:
		parts := make([]string, 0, len(t.Members))
		for _, m := range t.Members {
			mt, err := l.describe(m.Type, depth+1)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("%s@%s+%d:%s", m.Label, m.Slot, m.Offset, mt))
		}
		return "struct:" + t.NumberOfBytes + "{" + strings.Join(parts, ",") + "}", nil
	default:
		// Value types: the label names them fully (uint64, address, bool, an enum's
		// underlying size is in the label's bytes).
		return t.Label + ":" + t.NumberOfBytes, nil
	}
}

func (l *Layout) size(id string) (int, error) {
	t, ok := l.Types[id]
	if !ok {
		return 0, fmt.Errorf("storagelayout: unknown type %s", id)
	}
	n, err := strconv.Atoi(t.NumberOfBytes)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("storagelayout: type %s has size %q", id, t.NumberOfBytes)
	}
	return n, nil
}

// Compatible reports whether next keeps every variable of prev: the same name at the
// same slot and offset with the same type, and no new variable over its bytes. New
// variables may follow in free space.
func Compatible(prev, next []Entry) error {
	var errs []error
	for _, p := range prev {
		found := false
		for _, n := range next {
			if n.Slot == p.Slot && n.Offset == p.Offset {
				found = true
				if n.Label != p.Label {
					errs = append(errs, fmt.Errorf("%w: slot %s+%d holds %s, was %s", ErrIncompatible, hexSlot(p.Slot), p.Offset, n.Label, p.Label))
				} else if n.Type != p.Type || n.Bytes != p.Bytes {
					errs = append(errs, fmt.Errorf("%w: %s changed type: %s, was %s", ErrIncompatible, p.Label, n.Type, p.Type))
				}
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("%w: %s at slot %s+%d is gone", ErrIncompatible, p.Label, hexSlot(p.Slot), p.Offset))
		}
	}
	for _, n := range next {
		if kept(prev, n) {
			continue
		}
		for _, p := range prev {
			if overlap(p, n) {
				errs = append(errs, fmt.Errorf("%w: new %s at slot %s+%d overlaps %s", ErrIncompatible, n.Label, hexSlot(n.Slot), n.Offset, p.Label))
			}
		}
	}
	return errors.Join(errs...)
}

func kept(prev []Entry, n Entry) bool {
	for _, p := range prev {
		if p.Slot == n.Slot && p.Offset == n.Offset && p.Label == n.Label {
			return true
		}
	}
	return false
}

// overlap reports whether two variables share a byte. A variable of at most 32 bytes
// lies in its slot from its offset; a larger one fills whole slots from its own.
func overlap(a, b Entry) bool {
	aStart, aEnd := span(a)
	bStart, bEnd := span(b)
	return aStart.Cmp(bEnd) < 0 && bStart.Cmp(aEnd) < 0
}

// span is the byte range of a variable, counted from slot 0 in bytes.
func span(e Entry) (*big.Int, *big.Int) {
	slot, _ := new(big.Int).SetString(e.Slot, 10)
	start := new(big.Int).Mul(slot, big.NewInt(32))
	start.Add(start, big.NewInt(int64(e.Offset)))
	size := int64(e.Bytes)
	if e.Bytes > 32 {
		size = int64((e.Bytes + 31) / 32 * 32)
	}
	return start, new(big.Int).Add(start, big.NewInt(size))
}

// hexSlot writes a slot number in hex, as slots are usually read.
func hexSlot(slot string) string {
	n, ok := new(big.Int).SetString(slot, 10)
	if !ok {
		return slot
	}
	return "0x" + n.Text(16)
}

func less(a, b Entry) bool {
	sa, _ := new(big.Int).SetString(a.Slot, 10)
	sb, _ := new(big.Int).SetString(b.Slot, 10)
	if c := sa.Cmp(sb); c != 0 {
		return c < 0
	}
	return a.Offset < b.Offset
}
