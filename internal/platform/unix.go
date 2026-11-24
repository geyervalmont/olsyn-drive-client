//go:build darwin || linux

package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// ExecutableName is the binary's file name on this OS.
const ExecutableName = "olsyn-drive"

// AcquireInstance takes an exclusive flock on <runtime>/agent.lock. The lock
// goes away with the process, so a crash never leaves it held.
func AcquireInstance(p Paths) (func(), error) {
	if err := os.MkdirAll(p.Runtime, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", p.Runtime, err)
	}
	path := filepath.Join(p.Runtime, "agent.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// FreeDiskBytes is the space available to this user on the volume holding path.
func FreeDiskBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("free space of %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// DownloadsDir is ~/Downloads.
func DownloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home folder: %w", err)
	}
	return filepath.Join(home, "Downloads"), nil
}

// StartDetached starts exe in a new session so it outlives the caller.
func StartDetached(exe string, args []string) (*os.Process, error) {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", exe, err)
	}
	return cmd.Process, nil
}
