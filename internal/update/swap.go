package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

// Swap is an executable replaced by a new version, with the old one kept
// beside it as <exe>.old until the new one is known to be healthy.
type Swap struct {
	Exe string
	Old string
}

// Install puts staged in place of exe. The new file is first copied next to
// exe (same volume, so the final step is a rename). On Windows the running
// exe is renamed aside (allowed while it runs) and the new one renamed in; on
// unix the old file is hard-linked aside and the new one renamed over it
// atomically.
func Install(exe, staged string) (*Swap, error) {
	s := &Swap{Exe: exe, Old: exe + ".old"}
	next := exe + ".new"
	if err := copyFile(staged, next); err != nil {
		return nil, fmt.Errorf("copy update next to the program: %w", err)
	}
	if err := removeIfExists(s.Old); err != nil {
		os.Remove(next)
		return nil, fmt.Errorf("remove previous backup: %w", err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(exe, s.Old); err != nil {
			os.Remove(next)
			return nil, fmt.Errorf("move running program aside: %w", err)
		}
		if err := os.Rename(next, exe); err != nil {
			os.Rename(s.Old, exe)
			os.Remove(next)
			return nil, fmt.Errorf("move update in: %w", err)
		}
		return s, nil
	}
	if err := os.Link(exe, s.Old); err != nil {
		if err := copyFile(exe, s.Old); err != nil {
			os.Remove(next)
			return nil, fmt.Errorf("keep a backup of the program: %w", err)
		}
	}
	if err := os.Rename(next, exe); err != nil {
		os.Remove(next)
		return nil, fmt.Errorf("move update in: %w", err)
	}
	return s, nil
}

// Rollback puts the old executable back. The failed one is left as
// <exe>.failed on Windows (it may still be locked) and removed on unix.
func (s *Swap) Rollback() error {
	if runtime.GOOS == "windows" {
		failed := s.Exe + ".failed"
		removeIfExists(failed)
		if err := os.Rename(s.Exe, failed); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("move failed version aside: %w", err)
		}
	}
	if err := os.Rename(s.Old, s.Exe); err != nil {
		return fmt.Errorf("restore previous version: %w", err)
	}
	return nil
}

// CleanupOld removes what earlier updates left next to exe. Called on a
// healthy start; files still locked (Windows) are left for the next start.
func CleanupOld(exe string) error {
	var errs []error
	for _, suffix := range []string{".old", ".failed", ".new"} {
		if err := removeIfExists(exe + suffix); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm()|0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
