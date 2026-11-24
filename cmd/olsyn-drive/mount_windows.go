package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/geyervalmont/olsyn-drive-client/internal/core"
	"github.com/geyervalmont/olsyn-drive-client/internal/mount/winfsp"
)

// newMounter mounts the core's file system with WinFsp as a network drive.
func newMounter(c *core.FS, log *slog.Logger) *mounter {
	m := &mounter{core: c, log: log}
	m.mount = func(ctx context.Context, letter string) (string, func(), error) {
		v := c.VFS()
		if v == nil {
			return "", nil, fmt.Errorf("the drive is not ready yet")
		}
		letter = strings.ToUpper(strings.TrimSuffix(letter, "\\"))
		vol, err := winfsp.Mount(v, winfsp.Options{MountPoint: letter, VolumePrefix: `\olsyn\drive`, MarkOffline: true,
			Logger: log.With("component", "winfsp")})
		if err != nil {
			return "", nil, err
		}
		return letter + "\\", vol.Unmount, nil
	}
	return m
}
