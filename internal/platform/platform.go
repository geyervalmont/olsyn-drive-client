// Package platform holds the operating-system specific pieces of the client:
// the local IPC transport, the credential store, autostart, single instance,
// per-user paths, opening URLs and folders, URL scheme and uninstall
// registration, file system driver detection and installation, and disks.
//
// Each feature has a Windows implementation (the first platform), and macOS
// and Linux implementations or stubs that return ErrNotImplemented. Nothing in
// this package keeps state between calls.
package platform

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProductName is the folder and display name used on every platform.
const ProductName = "Olsyn Drive"

var (
	// ErrNotImplemented is returned by features this OS does not have yet.
	ErrNotImplemented = errors.New("not implemented on this operating system")
	// ErrNotFound is returned by a credential store for a missing secret.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyRunning means another agent of the same user holds the lock or endpoint.
	ErrAlreadyRunning = errors.New("another olsyn-drive agent is already running for this user")
	// ErrDeclined means the person declined an elevation prompt.
	ErrDeclined = errors.New("the person declined the administrator prompt")
	// ErrRestartNeeded means the driver installed but Windows must restart
	// (or the person sign out and in) before it can be used.
	ErrRestartNeeded = errors.New("the driver is installed but needs a restart of Windows")
)

// Paths are the per-user folders the client uses.
type Paths struct {
	Config  string // settings.json, account.json
	Cache   string // default content cache location
	Logs    string // agent and UI logs, diagnostics bundles
	State   string // update staging, anti-replay state, journals
	Runtime string // IPC socket and lock files (unix); unused on Windows
}

// Ensure creates every folder with owner-only permissions.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.Config, p.Cache, p.Logs, p.State, p.Runtime} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// Under returns Paths rooted at base, for tests and portable runs.
func Under(base string) Paths {
	return Paths{
		Config:  filepath.Join(base, "config"),
		Cache:   filepath.Join(base, "cache"),
		Logs:    filepath.Join(base, "logs"),
		State:   filepath.Join(base, "state"),
		Runtime: filepath.Join(base, "run"),
	}
}

// CredentialStore keeps secrets (the device token) in the OS store.
type CredentialStore interface {
	Get(target string) ([]byte, error)
	Set(target string, secret []byte) error
	Delete(target string) error
}

// CredentialChecker is a CredentialStore that can tell before a sign-in
// whether it can keep a secret at all. Windows Credential Manager cannot in a
// network logon session (an SSH session, say): a sign-in there must be refused
// at once rather than approved in the browser and then lost.
type CredentialChecker interface {
	CheckWritable() error
}

// CredentialTarget is the credential name for a drive server:
// "Olsyn Drive/<host>".
func CredentialTarget(serverURL string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parse server url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server url %q has no host", serverURL)
	}
	return ProductName + "/" + strings.ToLower(u.Host), nil
}

// DriverInfo describes the file system driver the mount needs (WinFsp on Windows).
type DriverInfo struct {
	Name       string // "winfsp"
	Needed     bool   // false where no driver is needed (or not supported yet)
	Installed  bool
	Version    string // dotted file version, e.g. "2.2.25089"
	InstallDir string
	// Others names other user-mode file system providers found on this
	// computer ("dokany"). They coexist with WinFsp; this is reported only.
	Others []string
}

// Steps InstallDriver reports through its progress callback, in order.
const (
	DriverStepPreparing  = "preparing"  // extracting and checking the installer
	DriverStepApproval   = "approval"   // the administrator prompt is up
	DriverStepInstalling = "installing" // the installer is running
)

// UninstallEntry is what the OS's "installed apps" list shows.
type UninstallEntry struct {
	DisplayName      string
	DisplayVersion   string
	Publisher        string
	InstallLocation  string
	UninstallCommand string
	DisplayIcon      string
	EstimatedSizeKB  uint32
}

// Name returns the release platform name of this build, as release manifests
// use it: windows-x64, windows-arm64, macos-arm64, macos-x64, linux-x64, linux-arm64.
func Name() string {
	return PlatformName(runtime.GOOS, runtime.GOARCH)
}

// PlatformName maps GOOS/GOARCH to a release platform name.
func PlatformName(goos, goarch string) string {
	osName := goos
	if goos == "darwin" {
		osName = "macos"
	}
	arch := goarch
	if goarch == "amd64" {
		arch = "x64"
	}
	return osName + "-" + arch
}

// OSFamily is the coarse platform a device reports to the drive: windows, macos or linux.
func OSFamily() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

// ComputerName is the name shown for this device on the drive.
func ComputerName() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "computer"
	}
	return strings.TrimSuffix(name, ".local")
}

// ValidateOpenURL accepts only web URLs, so nothing a caller passes can start
// an arbitrary program through the OS URL handlers.
func ValidateOpenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	switch u.Scheme {
	case "https", "http":
		if u.Host == "" {
			return fmt.Errorf("url has no host")
		}
		return nil
	default:
		return fmt.Errorf("refusing to open a %q url", u.Scheme)
	}
}

// writeFileAtomic writes data to path through a temporary file in the same folder.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	return nil
}
