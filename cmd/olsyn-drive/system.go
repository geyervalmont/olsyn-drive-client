package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// osSystem is agent.System over the platform package.
type osSystem struct {
	exe   string
	paths platform.Paths
}

func (s osSystem) OpenURL(ctx context.Context, u string) error    { return platform.OpenURL(ctx, u) }
func (s osSystem) OpenFolder(ctx context.Context, p string) error { return platform.OpenFolder(ctx, p) }

func (s osSystem) SetAutostart(on bool) error {
	if on {
		return platform.EnableAutostart(s.exe, []string{"agent"})
	}
	return platform.DisableAutostart()
}

func (s osSystem) DetectDriver() (platform.DriverInfo, error) { return platform.DetectDriver() }

// InstallDriver runs the WinFsp MSI shipped next to the executable, pinned by
// the SHA-256 compiled into this build.
func (s osSystem) InstallDriver(ctx context.Context) error {
	if winfspSHA256 == "" {
		return errors.New("this build carries no driver installer; install WinFsp from its publisher")
	}
	msi := filepath.Join(filepath.Dir(s.exe), "winfsp.msi")
	return platform.InstallDriver(ctx, msi, winfspSHA256, filepath.Join(s.paths.State, "driver"))
}

// ShowUI starts the desktop app with an olsyn-drive://show/<route> link; a
// running app (single instance) takes the link and shows that route.
func (s osSystem) ShowUI(ctx context.Context, route string) error {
	if !hasUI {
		return agent.ErrNoUI
	}
	_, err := platform.StartDetached(s.exe, []string{"olsyn-drive://show/" + route})
	return err
}

func (s osSystem) FreeDriveLetters() ([]string, error)       { return platform.FreeDriveLetters() }
func (s osSystem) DownloadsDir() (string, error)             { return platform.DownloadsDir() }
func (s osSystem) FreeDiskBytes(path string) (uint64, error) { return platform.FreeDiskBytes(path) }
