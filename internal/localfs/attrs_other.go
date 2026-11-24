//go:build !windows

package localfs

import (
	"io/fs"
	"os"
	"syscall"
)

// Attrs are what the platform says about a file beyond its mode.
type Attrs struct {
	Reparse     bool // Windows reparse point (junction, link, cloud file)
	Cloud       bool // Windows cloud-sync file or folder
	Placeholder bool // cloud "online-only" file: reading it downloads it
	Hidden      bool
	Links       uint64 // hard links (a spool refuses to delete a file with more than one)
}

// AttrsOf reads platform attributes. On macOS and Linux only the link
// count matters (cloud placeholders there are FUSE/File Provider files the
// walk reads like any other).
func AttrsOf(_ string, info fs.FileInfo) Attrs {
	var a Attrs
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		a.Links = uint64(st.Nlink)
	}
	return a
}

// OpenExclusive reports whether a file can be opened for reading with no
// other writer. Unix has no share modes: a writer is detected by size and
// time settling instead (spool), so this only checks it opens.
func OpenExclusive(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	return f.Close()
}

// IsLocked: never on Unix (no mandatory locks the client can see).
func IsLocked(error) bool { return false }
