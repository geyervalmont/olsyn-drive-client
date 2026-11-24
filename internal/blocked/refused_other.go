//go:build !windows

package blocked

// refusedBySecurity: other systems report a refused start as permission
// denied, which has other causes too; only a missing program is recognised.
func refusedBySecurity(error) bool { return false }
