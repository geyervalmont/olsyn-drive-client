// Package fsx is the client's view of the local file system: which entries
// are real files to send, which are junk or still being written, and which
// paths are safe to touch. Nothing here follows a link out of its root.
package fsx

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxName is the longest name (bytes) the client sends; the drive's own
// limit is the same as most file systems'.
const MaxName = 255

// MaxDepth bounds how deep a walk goes (a loop or a hostile tree).
const MaxDepth = 64

// Junk is never sent and, in a spool, cleared once nothing else is left
// beside it: OS litter and app caches.
var junkNames = map[string]bool{
	".ds_store": true, "thumbs.db": true, "desktop.ini": true, "ehthumbs.db": true, "ehthumbs_vista.db": true,
	".localized": true, "icon\r": true, ".spotlight-v100": true, ".trashes": true, ".fseventsd": true,
	".temporaryitems": true, ".apdisk": true, "$recycle.bin": true, "system volume information": true,
	".olsyn-drive": true,
}

// IsJunk: OS litter that is never anyone's work.
func IsJunk(name string) bool {
	l := strings.ToLower(name)
	return junkNames[l] || strings.HasPrefix(name, "._") || strings.HasPrefix(l, "spool full - ")
}

// IsTemp: a name apps and copy tools give a file while they're still
// writing it. It's waited for (it will be renamed), never sent.
func IsTemp(name string) bool {
	l := strings.ToLower(name)
	switch {
	case strings.HasPrefix(name, "~$"), // Office owner files
		strings.HasPrefix(l, ".~lock."), // LibreOffice
		strings.HasPrefix(name, ".~"),
		strings.HasPrefix(l, ".smbdelete"), // Samba pending deletes
		strings.HasPrefix(l, ".nfs"),
		strings.HasPrefix(l, "~wrl"), // Office temp
		strings.HasSuffix(l, ".tmp"), strings.HasSuffix(l, ".temp"),
		strings.HasSuffix(l, ".part"), strings.HasSuffix(l, ".partial"),
		strings.HasSuffix(l, ".crdownload"), strings.HasSuffix(l, ".download"),
		strings.HasSuffix(l, ".!ut"), strings.HasSuffix(l, ".filepart"),
		strings.HasSuffix(l, ".swp"), strings.HasSuffix(l, ".swx"),
		strings.HasSuffix(l, "~"),
		strings.HasPrefix(name, ".") && strings.Contains(name, ".sb-"): // macOS safe-save
		return true
	}
	return false
}

// windowsReserved are device names Windows won't open as files, with any
// extension ("CON.txt" too).
var windowsReserved = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
	"com¹": true, "com²": true, "com³": true, "lpt¹": true, "lpt²": true, "lpt³": true, "conin$": true, "conout$": true}

// CheckName says why a name can't be sent as it is, or "" when it can. It
// is the client's side of the drive's name rules; the drive still checks.
func CheckName(name string) string {
	switch {
	case name == "" || name == "." || name == "..":
		return "an empty or dot name"
	case len(name) > MaxName:
		return fmt.Sprintf("the name is longer than %d bytes", MaxName)
	case !utf8.ValidString(name):
		return "the name isn't valid text (UTF-8)"
	case strings.ContainsAny(name, "/\\"):
		return "the name contains a slash"
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cs, r) {
			return "the name contains a control character"
		}
	}
	if strings.TrimSpace(name) != name || strings.HasSuffix(name, ".") {
		return "the name starts or ends with a space, or ends with a dot"
	}
	return ""
}

// WindowsNameProblem says why Windows would refuse a name (reserved device
// names, characters it forbids); "" when it's fine. Used when listing
// what a send would do on Windows and to warn on other systems.
func WindowsNameProblem(name string) string {
	base := strings.ToLower(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.TrimRight(base, " ")] {
		return fmt.Sprintf("%q is a reserved name on Windows", name)
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return "the name contains a character Windows forbids"
	}
	return ""
}

// Rel turns a path below root into the "/"-separated relative path the
// drive sees, refusing anything that would leave root.
func Rel(root, p string) (string, error) {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	r = filepath.ToSlash(r)
	if r == "." || r == ".." || strings.HasPrefix(r, "../") || path.IsAbs(r) || filepath.IsAbs(r) {
		return "", fmt.Errorf("%q is outside %q", p, root)
	}
	return r, nil
}

// Join puts a relative "/" path back below root, refusing traversal.
func Join(root, rel string) (string, error) {
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || clean != rel || strings.Contains(rel, "\\") {
		return "", fmt.Errorf("unsafe relative path %q", rel)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// Entry is one regular file found by a walk.
type Entry struct {
	Abs     string
	Rel     string // "/"-separated, below the walk's root
	Size    int64
	MtimeNs int64
	Info    Attrs
}

// Skip is something a walk saw and won't send, with why.
type Skip struct {
	Rel    string
	Reason string
}

// WalkOptions shape a walk.
type WalkOptions struct {
	// Placeholders: what to do with cloud placeholders (OneDrive "online
	// only"): "skip" (default, listed in the plan) or "include" (reading
	// them makes OneDrive download them first).
	Placeholders string
}

// Walk lists the regular files below root, never following links,
// junctions or other reparse points, and reports what it skipped. Junk is
// skipped silently; temp names are reported (they're still being written).
func Walk(root string, opt WalkOptions, fn func(Entry) error) ([]Skip, error) {
	var skips []Skip
	root = filepath.Clean(root)
	st, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%q is not a folder", root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if p == root {
			return err
		}
		rel, rerr := Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if err != nil {
			skips = append(skips, Skip{rel, "can't be read: " + plainErr(err)})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if IsJunk(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.Count(rel, "/") >= MaxDepth {
			skips = append(skips, Skip{rel, "too deep"})
			return fs.SkipDir
		}
		if d.Type()&fs.ModeSymlink != 0 {
			skips = append(skips, Skip{rel, "a link (links are never followed)"})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			skips = append(skips, Skip{rel, "can't be read: " + plainErr(err)})
			return nil
		}
		attrs := AttrsOf(p, info)
		if attrs.Reparse && !attrs.Cloud {
			skips = append(skips, Skip{rel, "a junction or link (never followed)"})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if why := CheckName(name); why != "" {
			skips = append(skips, Skip{rel, why})
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() && !attrs.Cloud {
			skips = append(skips, Skip{rel, "not a regular file"})
			return nil
		}
		if IsTemp(name) {
			skips = append(skips, Skip{rel, "a temporary file (still being written?)"})
			return nil
		}
		if attrs.Placeholder && opt.Placeholders != "include" {
			skips = append(skips, Skip{rel, "online-only (cloud placeholder): make it available offline, or pass --include-online-only"})
			return nil
		}
		return fn(Entry{Abs: p, Rel: rel, Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), Info: attrs})
	})
	return skips, err
}

func plainErr(err error) string {
	var pe *fs.PathError
	if e, ok := err.(*fs.PathError); ok {
		pe = e
		return pe.Err.Error()
	}
	return err.Error()
}
