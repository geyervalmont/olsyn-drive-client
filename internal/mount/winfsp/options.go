// Package winfsp mounts a vfs.FileSystem as a Windows drive through WinFsp
// (https://winfsp.dev). It is a thin adapter: every WinFsp callback is
// translated into a vfs call, and vfs error kinds into NTSTATUS codes. All
// behaviour (caching, queueing, offline handling, name mapping) lives in vfs.
//
// The adapter only builds on Windows; elsewhere Mount returns
// errors.ErrUnsupported.
package winfsp

import "log/slog"

// Options configure a mount.
type Options struct {
	// MountPoint is a drive letter ("W:") or an empty NTFS folder.
	MountPoint string
	// FileSystemName is what Windows shows as the file system type.
	// Default "OlsynDrive".
	FileSystemName string
	// VolumePrefix, when set ("\server\share"), mounts as a network drive.
	VolumePrefix string
	// MarkOffline sets FILE_ATTRIBUTE_OFFLINE on files whose content is not
	// on this PC, so Explorer does not open them for thumbnails or previews.
	MarkOffline bool
	// Debug turns on WinFsp's own debug log.
	Debug bool
	// Logger receives one line per refused or failed callback; nil discards.
	Logger *slog.Logger
}
