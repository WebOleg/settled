// Package awscreds reads an AWS access key from a file, as the devnet's bridge-init.sh
// writes it for each principal of the KMS: the access key id on the first line, the
// secret on the second. Neither is ever repeated in an error.
package awscreds

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var accessKeyID = regexp.MustCompile(`^AKIA[0-9A-Z]{16}$`)

// Read reads the access key id and its secret from path.
func Read(path string) (id, secret string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("awscreds: read the credentials: %w", err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) != 2 || !accessKeyID.MatchString(lines[0]) || len(lines[1]) < 32 {
		return "", "", errors.New("awscreds: the credentials file does not hold an access key id and a secret of 32 characters or more, one per line")
	}
	return lines[0], lines[1], nil
}
