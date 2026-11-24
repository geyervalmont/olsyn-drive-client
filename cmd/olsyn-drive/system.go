package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/winfspmsi"
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

// InstallDriver extracts the WinFsp MSI embedded in this program to a new
// temporary folder and runs it elevated; platform.InstallDriver checks its
// SHA-256 against the pin and its Authenticode signature first.
func (s osSystem) InstallDriver(ctx context.Context, progress func(string)) error {
	msi := winfspmsi.Bytes()
	if msi == nil {
		return errors.New("this build carries no driver installer; install WinFsp " + winfspmsi.Version + " from its publisher")
	}
	dir, err := os.MkdirTemp("", "olsyn-drive-winfsp-")
	if err != nil {
		return fmt.Errorf("create a temporary folder for the driver installer: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, winfspmsi.File)
	if err := os.WriteFile(path, msi, 0o600); err != nil {
		return fmt.Errorf("extract the driver installer: %w", err)
	}
	return platform.InstallDriver(ctx, path, winfspmsi.SHA256, dir, progress)
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

func (s osSystem) DriveLettersInUse() (map[string]string, error) { return platform.DriveLettersInUse() }
func (s osSystem) DownloadsDir() (string, error)                 { return platform.DownloadsDir() }
func (s osSystem) FreeDiskBytes(path string) (uint64, error)     { return platform.FreeDiskBytes(path) }
