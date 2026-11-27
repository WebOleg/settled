package main

import "testing"

func TestLogins_acceptOnlyPlainNames(t *testing.T) {
	var ls logins
	if err := ls.Set("indexer:deposit_indexer:/run/pw"); err != nil || len(ls) != 1 || ls[0].role != "deposit_indexer" {
		t.Fatalf("a good login: %v, %+v", err, ls)
	}
	for _, bad := range []string{"indexer:deposit_indexer", "Indexer:r:/f", "x;drop:r:/f", "a:b'c:/f", "a:b:", ":b:/f"} {
		if err := ls.Set(bad); err == nil {
			t.Fatalf("-login %q accepted", bad)
		}
	}
}
