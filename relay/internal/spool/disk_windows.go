//go:build windows

package spool

import (
	"runtime"
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeBytes is the space left for this user on the spool's disk (-1 when
// unknown).
func freeBytes(dir string) int64 {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var avail, total, free uint64
	r, _, _ := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return -1
	}
	return int64(avail)
}

// lockDir: Windows has no simple equivalent of a read-only folder for
// SMB clients (the read-only bit on a folder is ignored by Explorer).
// The marker file says the spool is full; an operator who needs a hard
// stop sets a File Server Resource Manager quota on the folder.
func lockDir(string, bool) {}

func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }
