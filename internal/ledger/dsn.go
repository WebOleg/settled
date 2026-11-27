package ledger

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// The passwords the init script makes are hex: nothing in them needs quoting.
var hexPassword = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// ReadPassword reads a password from file: hex, 32 to 128 characters.
func ReadPassword(file string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("ledger: read the password: %w", err)
	}
	pw := strings.TrimSpace(string(raw))
	if !hexPassword.MatchString(pw) {
		return "", errors.New("ledger: the password file does not hold hex of 32 to 128 characters")
	}
	return pw, nil
}

// DSN puts the password from file into a postgres:// URL that names a user and carries
// no password of its own, so a password never sits in a flag or a process list.
func DSN(raw, passwordFile string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.User.Username() == "" {
		return "", errors.New("ledger: the database URL must be postgres:// with a user name")
	}
	if _, set := u.User.Password(); set {
		return "", errors.New("ledger: the database URL must not carry a password; it is read from a file")
	}
	pw, err := ReadPassword(passwordFile)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword(u.User.Username(), pw)
	return u.String(), nil
}
