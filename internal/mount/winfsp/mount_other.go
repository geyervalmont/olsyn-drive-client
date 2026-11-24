//go:build !windows

package winfsp

import (
	"errors"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// Volume is a mounted drive.
type Volume struct{}

// Mount is only available on Windows.
func Mount(vfs.FileSystem, Options) (*Volume, error) {
	return nil, errors.ErrUnsupported
}

// Unmount does nothing outside Windows.
func (*Volume) Unmount() {}
