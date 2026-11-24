//go:build linux

package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultPaths follows the XDG base directory conventions.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("find home folder: %w", err)
	}
	xdg := func(env, fallback string) string {
		if v := os.Getenv(env); filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, fallback)
	}
	state := filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "olsyn-drive")
	runtimeDir := filepath.Join(state, "run")
	if v := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(v) {
		runtimeDir = filepath.Join(v, "olsyn-drive")
	}
	return Paths{
		Config:  filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "olsyn-drive"),
		Cache:   filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "olsyn-drive"),
		Logs:    filepath.Join(state, "logs"),
		State:   state,
		Runtime: runtimeDir,
	}, nil
}

// DefaultInstallDir is ~/.local/share/olsyn-drive.
func DefaultInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home folder: %w", err)
	}
	return filepath.Join(home, ".local", "share", "olsyn-drive"), nil
}

func autostartPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home folder: %w", err)
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "olsyn-drive.desktop"), nil
}

// EnableAutostart writes an XDG autostart entry.
func EnableAutostart(exe string, args []string) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	parts := []string{desktopQuote(exe)}
	for _, a := range args {
		parts = append(parts, desktopQuote(a))
	}
	entry := "[Desktop Entry]\nType=Application\nName=" + ProductName + "\nExec=" + strings.Join(parts, " ") +
		"\nX-GNOME-Autostart-enabled=true\nNoDisplay=true\n"
	return writeFileAtomic(path, []byte(entry), 0o644)
}

// DisableAutostart removes the autostart entry.
func DisableAutostart() error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove autostart entry: %w", err)
	}
	return nil
}

// AutostartEnabled reports whether the autostart entry exists.
func AutostartEnabled() (bool, error) {
	path, err := autostartPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// OpenURL opens a web URL with xdg-open.
func OpenURL(ctx context.Context, rawURL string) error {
	if err := ValidateOpenURL(rawURL); err != nil {
		return err
	}
	return run(ctx, "xdg-open", rawURL)
}

// OpenFolder opens a folder in the file manager with xdg-open.
func OpenFolder(ctx context.Context, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("open folder: %s is not a folder", path)
	}
	return run(ctx, "xdg-open", path)
}

func run(ctx context.Context, name string, args ...string) error {
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

// desktopQuote quotes an Exec argument per the desktop entry specification.
func desktopQuote(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$", "%", "%%")
	return "\"" + r.Replace(s) + "\""
}
