//go:build !windows

package spool

import (
	"os"
	"runtime"
	"syscall"
)

// freeBytes is the space left for this user on the spool's disk (-1 when
// unknown).
func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// lockDir takes group and other write access off a drop folder while the
// spool is full, so copies from file-sharing users (who write through the
// sharing group, not as this node's own user) fail at once with
// "permission denied"; the node itself can still delete what it sent.
// It is given back when there's room again.
func lockDir(dir string, locked bool) {
	if locked {
		os.Chmod(dir, 0o755)
	} else {
		os.Chmod(dir, 0o775)
	}
}

func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }
