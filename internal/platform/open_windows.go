package platform

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// OpenURL opens a web URL in the default browser.
func OpenURL(_ context.Context, rawURL string) error {
	if err := ValidateOpenURL(rawURL); err != nil {
		return err
	}
	return shellOpen(rawURL)
}

// OpenFolder shows a folder in Explorer.
func OpenFolder(_ context.Context, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("open folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("open folder: %s is not a folder", path)
	}
	return shellOpen(path)
}

func shellOpen(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}
	if err := windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return fmt.Errorf("shell open: %w", err)
	}
	return nil
}
