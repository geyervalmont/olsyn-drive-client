//go:build windows

package winfsp

import (
	"errors"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
	"golang.org/x/sys/windows"
)

// kindStatus is the NTSTATUS each vfs error kind becomes. Offline is
// STATUS_NETWORK_UNREACHABLE: Explorer and Office show "the network
// location cannot be reached" at once instead of hanging.
var kindStatus = map[vfs.Kind]windows.NTStatus{
	vfs.KindOther:            windows.STATUS_UNEXPECTED_IO_ERROR,
	vfs.KindNotFound:         windows.STATUS_OBJECT_NAME_NOT_FOUND,
	vfs.KindExists:           windows.STATUS_OBJECT_NAME_COLLISION,
	vfs.KindNotDir:           windows.STATUS_NOT_A_DIRECTORY,
	vfs.KindIsDir:            windows.STATUS_FILE_IS_A_DIRECTORY,
	vfs.KindNotEmpty:         windows.STATUS_DIRECTORY_NOT_EMPTY,
	vfs.KindAccessDenied:     windows.STATUS_ACCESS_DENIED,
	vfs.KindSharingViolation: windows.STATUS_SHARING_VIOLATION,
	vfs.KindOffline:          windows.STATUS_NETWORK_UNREACHABLE,
	vfs.KindTimeout:          windows.STATUS_IO_TIMEOUT,
	vfs.KindNoSpace:          windows.STATUS_DISK_FULL,
	vfs.KindInvalidName:      windows.STATUS_OBJECT_NAME_INVALID,
	vfs.KindSignedOut:        windows.STATUS_NETWORK_ACCESS_DENIED,
	vfs.KindInvalid:          windows.STATUS_INVALID_PARAMETER,
}

// toStatus converts an error from vfs (or from this adapter) to an NTSTATUS.
func toStatus(err error) windows.NTStatus {
	if err == nil {
		return windows.STATUS_SUCCESS
	}
	var st windows.NTStatus
	if errors.As(err, &st) {
		return st
	}
	var ve *vfs.Error
	if errors.As(err, &ve) {
		if st, ok := kindStatus[ve.Kind]; ok {
			return st
		}
	}
	return windows.STATUS_UNEXPECTED_IO_ERROR
}
