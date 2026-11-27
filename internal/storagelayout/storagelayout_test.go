package storagelayout

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// vault reads the layout of the vault as forge built it (testdata/vault.json) and lets
// a test change it as JSON before parsing.
func vault(t *testing.T, change func(l map[string]any)) []Entry {
	t.Helper()
	raw, err := os.ReadFile("testdata/vault.json")
	if err != nil {
		t.Fatal(err)
	}
	var l map[string]any
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(l)
	}
	out, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func storage(l map[string]any) []any { return l["storage"].([]any) }

func variableNamed(l map[string]any, label string) map[string]any {
	for _, v := range storage(l) {
		if m := v.(map[string]any); m["label"] == label {
			return m
		}
	}
	panic("no variable " + label)
}

func TestTheVaultParses(t *testing.T) {
	entries := vault(t, nil)
	if len(entries) != 6 {
		t.Fatalf("%d variables, want 6", len(entries))
	}
	if entries[0].Label != "_tokens" || entries[5].Label != "_withdrawalDelay" || entries[5].Bytes != 8 {
		t.Fatalf("order or sizes wrong: %+v", entries)
	}
	if !strings.Contains(entries[1].Type, "level@0+0:uint128:16") {
		t.Fatalf("the bucket's members are not written out: %s", entries[1].Type)
	}
}

func TestSameLayoutIsCompatible(t *testing.T) {
	if err := Compatible(vault(t, nil), vault(t, nil)); err != nil {
		t.Fatal(err)
	}
}

// AST ids change from build to build; the same layout under other ids is the same.
func TestOtherASTIdsAreTheSameLayout(t *testing.T) {
	raw, err := os.ReadFile("testdata/vault.json")
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.NewReplacer("51890", "99990", "53121", "88881").Replace(string(raw))
	next, err := Parse([]byte(renamed))
	if err != nil {
		t.Fatal(err)
	}
	if err := Compatible(vault(t, nil), next); err != nil {
		t.Fatal(err)
	}
}

func TestAppendedVariableIsCompatible(t *testing.T) {
	next := vault(t, func(l map[string]any) {
		last := variableNamed(l, "_withdrawalDelay")
		added := map[string]any{"label": "_added", "offset": 8, "slot": last["slot"], "type": "t_uint64"}
		l["storage"] = append(storage(l), added)
	})
	if err := Compatible(vault(t, nil), next); err != nil {
		t.Fatal(err)
	}
}

func TestChangesThatBreakTheState(t *testing.T) {
	cases := map[string]func(l map[string]any){
		"renamed": func(l map[string]any) { variableNamed(l, "_depositCount")["label"] = "_deposits" },
		"retyped": func(l map[string]any) { variableNamed(l, "_withdrawalDelay")["type"] = "t_uint256" },
		"moved": func(l map[string]any) {
			variableNamed(l, "_withdrawalDelay")["slot"] = variableNamed(l, "_depositCount")["slot"]
			variableNamed(l, "_withdrawalDelay")["offset"] = 8
		},
		"removed": func(l map[string]any) {
			kept := []any{}
			for _, v := range storage(l) {
				if v.(map[string]any)["label"] != "_accounted" {
					kept = append(kept, v)
				}
			}
			l["storage"] = kept
		},
		"a struct member retyped": func(l map[string]any) {
			for id, t := range l["types"].(map[string]any) {
				if strings.HasPrefix(id, "t_struct(Bucket)") {
					m := t.(map[string]any)["members"].([]any)[1].(map[string]any)
					m["type"] = "t_uint128"
				}
			}
		},
		"a new variable over an old one": func(l map[string]any) {
			last := variableNamed(l, "_withdrawalDelay")
			added := map[string]any{"label": "_over", "offset": 4, "slot": last["slot"], "type": "t_uint64"}
			l["storage"] = append(storage(l), added)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			err := Compatible(vault(t, nil), vault(t, change))
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("got %v, want ErrIncompatible", err)
			}
		})
	}
}

func TestMalformedInputIsRefused(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":     "{",
		"no storage":   `{"types": {}}`,
		"unknown type": `{"storage": [{"label": "x", "offset": 0, "slot": "0", "type": "t_missing"}], "types": {}}`,
		"bad slot":     `{"storage": [{"label": "x", "offset": 0, "slot": "zero", "type": "t_uint8"}], "types": {"t_uint8": {"encoding": "inplace", "label": "uint8", "numberOfBytes": "1"}}}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
