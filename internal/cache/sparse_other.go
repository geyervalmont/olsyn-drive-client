//go:build !windows

package cache

import "os"

// setSparse is a no-op: APFS, ext4 and friends keep unwritten ranges sparse.
func setSparse(*os.File) error { return nil }
