//go:build windows

package cache

import (
	"os"
	"syscall"
)

// fsctlSetSparse is FSCTL_SET_SPARSE.
const fsctlSetSparse = 0x000900c4

// setSparse marks a data file sparse so unfetched ranges take no disk space.
func setSparse(f *os.File) error {
	var returned uint32
	return syscall.DeviceIoControl(syscall.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &returned, nil)
}
