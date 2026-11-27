package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDSN_takesThePasswordFromTheFileOnly(t *testing.T) {
	pw := strings.Repeat("ab", 24)
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte(pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DSN("postgres://indexer@postgres:5432/ledger?sslmode=disable", file)
	if err != nil || got != "postgres://indexer:"+pw+"@postgres:5432/ledger?sslmode=disable" {
		t.Fatalf("DSN: %q, %v", got, err)
	}
	for name, raw := range map[string]string{
		"password in the URL": "postgres://indexer:other@postgres:5432/ledger",
		"no user":             "postgres://postgres:5432/ledger",
	} {
		if _, err := DSN(raw, file); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	for name, content := range map[string]string{"empty": "\n", "not hex": "s3cret' OR '1'='1", "short": "abcd"} {
		bad := filepath.Join(t.TempDir(), "bad")
		if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := DSN("postgres://indexer@postgres/ledger", bad); err == nil {
			t.Fatalf("%s password file: accepted", name)
		}
	}
	if _, err := DSN("postgres://indexer@postgres/ledger", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing password file was accepted")
	}
}
