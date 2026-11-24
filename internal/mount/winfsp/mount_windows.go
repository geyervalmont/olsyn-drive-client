//go:build windows

package winfsp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
	"github.com/winfsp/go-winfsp"
)

// Volume is a mounted drive.
type Volume struct {
	fs     *winfsp.FileSystem
	cancel context.CancelFunc
	a      *adapter
}

// Mount mounts fsys at o.MountPoint. WinFsp 2.x must be installed.
func Mount(fsys vfs.FileSystem, o Options) (*Volume, error) {
	if err := winfsp.LoadWinFSP(); err != nil {
		return nil, fmt.Errorf("winfsp: WinFsp is not installed: %w", err)
	}
	if o.FileSystemName == "" {
		o.FileSystemName = "OlsynDrive"
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := newAdapter(ctx, fsys, o.MarkOffline, log)
	if err != nil {
		cancel()
		return nil, err
	}
	opts := []winfsp.Option{
		winfsp.FileSystemName(o.FileSystemName),
		winfsp.CaseSensitive(false),
		winfsp.CasePreserveNames(true),
		winfsp.SectorSize(512, 8), // 4 KiB allocation unit
		winfsp.Debug(o.Debug),
		// answer FindFirstFile on one name without listing the folder
		winfsp.Attributes(winfsp.FspFSAttributePassQueryDirectoryFileName),
	}
	if o.VolumePrefix != "" {
		opts = append(opts, winfsp.VolumePrefix(o.VolumePrefix))
	}
	fs, err := winfsp.Mount(a, o.MountPoint, opts...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("winfsp: mount %s: %w", o.MountPoint, err)
	}
	return &Volume{fs: fs, cancel: cancel, a: a}, nil
}

// Unmount removes the drive. Open handles are closed by WinFsp first.
func (v *Volume) Unmount() {
	v.fs.Unmount()
	v.cancel()
	v.a.closeAll()
}
