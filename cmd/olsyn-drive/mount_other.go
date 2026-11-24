//go:build !windows

package main

import (
	"log/slog"

	"github.com/geyervalmont/olsyn-drive-client/internal/core"
)

// newMounter: no mount adapter on this system yet (FUSE comes later). The
// agent reports mounting as failed with errMountUnsupported; sending works.
func newMounter(c *core.FS, log *slog.Logger) *mounter { return &mounter{core: c, log: log} }
