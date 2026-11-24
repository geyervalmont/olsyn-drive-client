//go:build !windows

package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func tokenPath(dir string) string { return filepath.Join(dir, "token") }

// saveToken writes the token readable by this user only.
func saveToken(dir, tok string) error {
	return writePrivate(tokenPath(dir), []byte(tok))
}

// loadToken refuses a token file others can read: someone loosened it, or
// it was copied in carelessly.
func loadToken(dir string) (string, error) {
	p := tokenPath(dir)
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s can be read by other users; fix with: chmod 600 %q", p, p)
	}
	b, err := os.ReadFile(p)
	return strings.TrimSpace(string(b)), err
}
