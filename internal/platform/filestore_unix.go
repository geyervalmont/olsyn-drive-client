//go:build !windows

package platform

import (
	"fmt"
	"os"
)

// restrictToOwner sets 0700 on a folder or 0600 on a file.
func restrictToOwner(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
