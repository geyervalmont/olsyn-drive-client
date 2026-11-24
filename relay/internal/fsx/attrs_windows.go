//go:build windows

package fsx

import (
	"errors"
	"io/fs"
	"syscall"
)

// Attrs are what the platform says about a file beyond its mode.
type Attrs struct {
	Reparse     bool // reparse point: junction, symlink, mount point or cloud file
	Cloud       bool // a cloud-sync file or folder (OneDrive): a reparse point, but an ordinary one to read
	Placeholder bool // cloud "online-only" file (OneDrive): reading it downloads it
	Hidden      bool
	Links       uint64
}

// File attribute bits (winnt.h) that syscall doesn't name.
const (
	attrRecallOnOpen       = 0x00040000 // FILE_ATTRIBUTE_RECALL_ON_OPEN
	attrRecallOnDataAccess = 0x00400000 // FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS
	attrOffline            = 0x00001000 // FILE_ATTRIBUTE_OFFLINE
	attrUnpinned           = 0x00100000 // FILE_ATTRIBUTE_UNPINNED
)

// AttrsOf reads the Win32 attributes: a reparse point is never followed;
// a cloud placeholder (OneDrive Files On-Demand) is reported so a send can
// say "online-only" instead of silently downloading tens of GB.
func AttrsOf(p string, info fs.FileInfo) Attrs {
	var a Attrs
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return a
	}
	at := d.FileAttributes
	a.Hidden = at&syscall.FILE_ATTRIBUTE_HIDDEN != 0
	a.Placeholder = at&(attrRecallOnDataAccess|attrRecallOnOpen|attrOffline) != 0
	a.Reparse = at&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	if a.Reparse {
		// OneDrive files and folders carry a cloud reparse tag even when
		// fully downloaded; junctions and links carry others. Only the
		// directory listing (FindFirstFile) says which.
		a.Cloud = isCloudTag(reparseTag(p))
	}
	a.Links = 1
	return a
}

// IO_REPARSE_TAG_CLOUD and its sixteen variants (_1 .. _F): 0x9000x01A.
func isCloudTag(tag uint32) bool { return tag&0xFFFF0FFF == 0x9000001A }

func reparseTag(p string) uint32 {
	name, err := syscall.UTF16PtrFromString(longPath(p))
	if err != nil {
		return 0
	}
	var fd syscall.Win32finddata
	h, err := syscall.FindFirstFile(name, &fd)
	if err != nil {
		return 0
	}
	syscall.FindClose(h)
	return fd.Reserved0 // dwReserved0: the reparse tag when the reparse bit is set
}

// OpenExclusive opens for reading while refusing to share write access:
// it fails while any other program has the file open for writing (a copy
// still arriving, an app saving). A file someone only reads passes.
func OpenExclusive(p string) error {
	name, err := syscall.UTF16PtrFromString(longPath(p))
	if err != nil {
		return err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: p, Err: err}
	}
	return syscall.CloseHandle(h)
}

// ERROR_SHARING_VIOLATION and ERROR_LOCK_VIOLATION.
const (
	errSharingViolation syscall.Errno = 32
	errLockViolation    syscall.Errno = 33
)

// IsLocked: another program holds the file (a sharing or lock violation):
// skip it now, retry later, report it if it stays locked.
func IsLocked(err error) bool {
	return errors.Is(err, errSharingViolation) || errors.Is(err, errLockViolation)
}

// longPath gives an absolute path the \\?\ prefix so paths over 260
// characters open (Go's os package already does this for its own calls).
func longPath(p string) string {
	if len(p) < 248 || len(p) >= 4 && p[:4] == `\\?\` {
		return p
	}
	if len(p) >= 2 && p[0] == '\\' && p[1] == '\\' {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}
