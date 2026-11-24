//go:build !windows

package platform

import (
	"fmt"
	"os"
)

// RemoveAfterExit deletes dir now: a running executable can be unlinked on unix.
func RemoveAfterExit(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}
