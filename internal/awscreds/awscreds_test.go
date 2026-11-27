package awscreds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead_anAccessKeyAndItsSecretOnlyAndNeverInAnError(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// The access key id is put together here, so no whole one is written in the source.
	key := "AKIA" + "0123456789ABCDEF"
	secret := strings.Repeat("s3cr3t/+", 5)
	id, got, err := Read(write("good", key+"\n"+secret+"\n"))
	if err != nil || id != key || got != secret {
		t.Fatalf("Read: %q, %v", id, err)
	}
	for name, content := range map[string]string{
		"one line":     key + "\n",
		"not a key id": strings.ToLower(key) + "\n" + secret + "\n",
		"short secret": key + "\nshort-" + "secret\n",
		"three lines":  key + "\n" + secret + "\nmore\n",
	} {
		_, _, err := Read(write(strings.ReplaceAll(name, " ", "-"), content))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "short-secret") {
			t.Errorf("%s: the error repeats the secret", name)
		}
	}
	if _, _, err := Read(filepath.Join(dir, "none")); err == nil {
		t.Error("a missing file: accepted")
	}
}
