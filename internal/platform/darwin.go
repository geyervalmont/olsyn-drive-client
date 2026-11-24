//go:build darwin

package platform

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchAgentLabel = "com.olsyn.drive.agent"

// DefaultPaths follows the macOS conventions under ~/Library.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("find home folder: %w", err)
	}
	support := filepath.Join(home, "Library", "Application Support", ProductName)
	return Paths{
		Config:  support,
		Cache:   filepath.Join(home, "Library", "Caches", ProductName),
		Logs:    filepath.Join(home, "Library", "Logs", ProductName),
		State:   filepath.Join(support, "state"),
		Runtime: filepath.Join(support, "run"),
	}, nil
}

// DefaultInstallDir is ~/Applications/Olsyn Drive.
func DefaultInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home folder: %w", err)
	}
	return filepath.Join(home, "Applications", ProductName), nil
}

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home folder: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// EnableAutostart writes a LaunchAgent that runs exe with args at login.
func EnableAutostart(exe string, args []string) error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + launchAgentLabel + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range append([]string{exe}, args...) {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>RunAtLoad</key>\n\t<true/>\n</dict>\n</plist>\n")
	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

// DisableAutostart removes the LaunchAgent.
func DisableAutostart() error {
	path, err := launchAgentPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove launch agent: %w", err)
	}
	return nil
}

// AutostartEnabled reports whether the LaunchAgent exists.
func AutostartEnabled() (bool, error) {
	path, err := launchAgentPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// OpenURL opens a web URL in the default browser.
func OpenURL(ctx context.Context, rawURL string) error {
	if err := ValidateOpenURL(rawURL); err != nil {
		return err
	}
	return run(ctx, "open", rawURL)
}

// OpenFolder shows a folder in Finder.
func OpenFolder(ctx context.Context, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("open folder: %s is not a folder", path)
	}
	return run(ctx, "open", path)
}

func run(ctx context.Context, name string, args ...string) error {
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("run %s: %w", name, err)
	}
	return nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
