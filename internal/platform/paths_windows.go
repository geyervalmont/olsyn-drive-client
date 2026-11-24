package platform

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// DefaultPaths puts everything under %LOCALAPPDATA%\Olsyn Drive.
func DefaultPaths() (Paths, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return Paths{}, fmt.Errorf("find local app data folder: %w", err)
	}
	base := filepath.Join(local, ProductName)
	return Paths{
		Config: filepath.Join(base, "config"),
		Cache:  filepath.Join(base, "cache"),
		Logs:   filepath.Join(base, "logs"),
		State:  filepath.Join(base, "state"),
	}, nil
}

// DefaultInstallDir is the per-user program folder: %LOCALAPPDATA%\Programs\Olsyn Drive.
func DefaultInstallDir() (string, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", fmt.Errorf("find local app data folder: %w", err)
	}
	return filepath.Join(local, "Programs", ProductName), nil
}

// DownloadsDir is the person's Downloads folder.
func DownloadsDir() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		return "", fmt.Errorf("find downloads folder: %w", err)
	}
	return dir, nil
}

// ExecutableName is the binary's file name on this OS.
const ExecutableName = "olsyn-drive.exe"
