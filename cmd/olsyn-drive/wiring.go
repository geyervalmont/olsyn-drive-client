package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/core"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// The agent's Drive, FileSystem and Mounter: internal/core over driveapi,
// vfs, index, cache, journal and transfer, and the WinFsp mount on Windows
// (mount_windows.go; other systems say mounting is not supported yet).

// serverEnv names the developer-build server override.
const serverEnv = "OLSYN_DRIVE_SERVER"

// chooseServer returns the server the agent is pinned to: the product's
// drive in a release build; in a developer build the --server flag or
// OLSYN_DRIVE_SERVER when given, else "" (the settings decide).
func chooseServer(flagValue string) (string, error) {
	override := flagValue
	if override == "" {
		override = os.Getenv(serverEnv)
	}
	if !devBuild {
		if override != "" && !sameServer(override, settings.DefaultServerURL) {
			return "", fmt.Errorf("this is a release build: it connects only to %s (--server and %s need a developer build)",
				settings.DefaultServerURL, serverEnv)
		}
		return settings.DefaultServerURL, nil
	}
	return strings.TrimRight(override, "/"), nil
}

// allowServer is the build's server policy, applied to every drive client.
func allowServer(u string) error {
	if devBuild || sameServer(u, settings.DefaultServerURL) {
		return nil
	}
	return fmt.Errorf("this build connects only to %s, not %s", settings.DefaultServerURL, u)
}

func sameServer(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// newHTTPClient is shared by every drive call. No overall timeout: uploads,
// downloads and the change stream are long; contexts bound each call.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.MaxIdleConnsPerHost = 32
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: tr}
}

func userAgent() string { return "olsyn-drive/" + version + " (" + platform.Name() + ")" }

func newDriveFor(hc *http.Client, log *slog.Logger) func(serverURL string) agent.Drive {
	return func(serverURL string) agent.Drive {
		if err := allowServer(serverURL); err != nil {
			return core.RefusedDrive(err)
		}
		return core.NewDrive(serverURL, core.DriveOptions{HTTPClient: hc, UserAgent: userAgent(), Logger: log})
	}
}

func newCore(e *env, hc *http.Client, log *slog.Logger) (*core.FS, error) {
	return core.New(core.Options{StateDir: e.paths.State, PCName: platform.ComputerName(), HTTPClient: hc,
		UserAgent: userAgent(), Logger: log, AllowServer: allowServer})
}

// mounter adapts a platform mount of the core's file system to agent.Mounter.
type mounter struct {
	core  *core.FS
	log   *slog.Logger
	mount func(ctx context.Context, letter string) (path string, unmount func(), err error)

	unmount func()
}

var errMountUnsupported = errors.New("mounting the drive is not supported on this system yet (Windows only); use send, or the web app")

func (m *mounter) Mount(ctx context.Context, letter string) (string, error) {
	if m.mount == nil {
		return "", errMountUnsupported
	}
	if m.core.VFS() == nil {
		return "", errors.New("the drive is not ready yet")
	}
	p, unmount, err := m.mount(ctx, letter)
	if err != nil {
		return "", err
	}
	m.unmount = unmount
	m.core.SetMountDir(p)
	return p, nil
}

func (m *mounter) Unmount(ctx context.Context, force bool) error {
	if m.unmount != nil {
		m.unmount()
		m.unmount = nil
	}
	m.core.SetMountDir("")
	return nil
}

func (m *mounter) OpenFiles() int { return m.core.OpenFiles() }
