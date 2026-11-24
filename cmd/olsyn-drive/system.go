package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/links"
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

// driverDownloadTimeout bounds the WinFsp installer download (about 2 MB).
const driverDownloadTimeout = 5 * time.Minute

// InstallDriver downloads the official WinFsp installer from the WinFsp
// project's GitHub release into a new temporary folder, checking its size
// and SHA-256 against the pins; platform.InstallDriver checks the hash and
// its Authenticode signature again and runs it elevated. A download that
// does not work is platform.ErrDriverUnavailable: the person can install
// WinFsp from its own download page instead.
func (s osSystem) InstallDriver(ctx context.Context, progress func(platform.DriverProgress)) error {
	if progress == nil {
		progress = func(platform.DriverProgress) {}
	}
	dir, err := os.MkdirTemp("", "olsyn-drive-winfsp-")
	if err != nil {
		return fmt.Errorf("create a temporary folder for the driver installer: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, winfspmsi.File)
	progress(platform.DriverProgress{Step: platform.DriverStepDownloading, Total: winfspmsi.Size})
	f := winfspmsi.Fetcher{HTTP: &http.Client{Timeout: driverDownloadTimeout}}
	if err := f.Fetch(ctx, path, func(done, total int64) {
		progress(platform.DriverProgress{Step: platform.DriverStepDownloading, Done: done, Total: total})
	}); err != nil {
		return fmt.Errorf("%w: %w", platform.ErrDriverUnavailable, err)
	}
	return platform.InstallDriver(ctx, path, winfspmsi.SHA256, dir, func(step string) {
		progress(platform.DriverProgress{Step: step})
	})
}

// ShowUI starts the desktop app with an olsyn-drive://open/<screen> link; a
// running app (single instance) takes the link and shows that screen.
func (s osSystem) ShowUI(ctx context.Context, screen string) error {
	if !hasUI {
		return agent.ErrNoUI
	}
	_, err := platform.StartDetached(s.exe, []string{links.ScreenLink(screen)})
	return err
}

func (s osSystem) DriveLettersInUse() (map[string]string, error) { return platform.DriveLettersInUse() }
func (s osSystem) DownloadsDir() (string, error)                 { return platform.DownloadsDir() }
func (s osSystem) FreeDiskBytes(path string) (uint64, error)     { return platform.FreeDiskBytes(path) }
