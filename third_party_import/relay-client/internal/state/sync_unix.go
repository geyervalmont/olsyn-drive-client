//go:build !windows

package state

import "os"

// syncDir makes a rename durable (POSIX needs the directory synced).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
