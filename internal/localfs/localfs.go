// Package localfs is the client's view of the local file system when it
// sends files: which entries are real files to send, which are junk or
// still being written, which are cloud "online-only" placeholders, and
// which paths are safe to touch. Nothing here follows a link out of its
// root, and nothing here writes or deletes: sending only ever reads.
//
// Ported from the relay client's fsx package (the walk now streams one
// folder at a time so memory does not grow with the number of files).
package localfs

import (
	"context"
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

// Skip is something a walk saw and won't send, with why. NotLocal marks a
// cloud placeholder (online-only), which is counted apart.
type Skip struct {
	Rel      string
	Reason   string
	NotLocal bool
	Size     int64
}

// WalkOptions shape a walk.
type WalkOptions struct {
	// IncludeOnlineOnly reads cloud placeholders (OneDrive "online only"),
	// which makes the sync client download them first. Off by default:
	// they are reported as skipped and not local.
	IncludeOnlineOnly bool
	// Attrs replaces AttrsOf (tests).
	Attrs func(path string, info fs.FileInfo) Attrs
}

// Walk calls fn for each regular file below root and skip for what it
// will not send, one folder at a time (memory is bounded by the largest
// folder, not by the tree). It never follows links, junctions or other
// reparse points. Junk is skipped silently; temp names, names the drive
// would refuse and names that differ from a sibling only in case are
// reported. fn's error stops the walk.
func Walk(ctx context.Context, root string, opt WalkOptions, fn func(Entry) error, skip func(Skip)) error {
	root = filepath.Clean(root)
	st, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%q is not a folder", root)
	}
	attrs := opt.Attrs
	if attrs == nil {
		attrs = AttrsOf
	}
	return walkDir(ctx, root, "", 0, opt, attrs, fn, skip)
}

func walkDir(ctx context.Context, dir, rel string, depth int, opt WalkOptions, attrs func(string, fs.FileInfo) Attrs,
	fn func(Entry) error, skip func(Skip)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if rel == "" {
			return err
		}
		skip(Skip{Rel: rel, Reason: "can't be read: " + plainErr(err)})
		return nil
	}
	seen := make(map[string]string, len(entries)) // lower-case name → first name
	var subdirs []string
	for _, d := range entries {
		name := d.Name()
		r := name
		if rel != "" {
			r = rel + "/" + name
		}
		if IsJunk(name) {
			continue
		}
		p := filepath.Join(dir, name)
		if d.Type()&fs.ModeSymlink != 0 {
			skip(Skip{Rel: r, Reason: "a link (links are never followed)"})
			continue
		}
		info, err := d.Info()
		if err != nil {
			skip(Skip{Rel: r, Reason: "can't be read: " + plainErr(err)})
			continue
		}
		a := attrs(p, info)
		if a.Reparse && !a.Cloud {
			skip(Skip{Rel: r, Reason: "a junction or link (never followed)"})
			continue
		}
		if why := CheckName(name); why != "" {
			skip(Skip{Rel: r, Reason: why})
			continue
		}
		low := strings.ToLower(name)
		if first, ok := seen[low]; ok {
			skip(Skip{Rel: r, Reason: fmt.Sprintf("differs only in upper/lower case from %q, which the drive treats as the same name", first)})
			continue
		}
		seen[low] = name
		if d.IsDir() {
			if depth+1 >= MaxDepth {
				skip(Skip{Rel: r, Reason: "too deep"})
				continue
			}
			subdirs = append(subdirs, name)
			continue
		}
		if !info.Mode().IsRegular() && !a.Cloud {
			skip(Skip{Rel: r, Reason: "not a regular file"})
			continue
		}
		if IsTemp(name) {
			skip(Skip{Rel: r, Reason: "a temporary file (still being written?)"})
			continue
		}
		if a.Placeholder && !opt.IncludeOnlineOnly {
			skip(Skip{Rel: r, NotLocal: true, Size: info.Size(),
				Reason: "online-only (a cloud placeholder): make it available offline first, or include online-only files"})
			continue
		}
		if err := fn(Entry{Abs: p, Rel: r, Size: info.Size(), MtimeNs: info.ModTime().UnixNano(), Info: a}); err != nil {
			return err
		}
	}
	entries = nil
	for _, name := range subdirs {
		r := name
		if rel != "" {
			r = rel + "/" + name
		}
		if err := walkDir(ctx, filepath.Join(dir, name), r, depth+1, opt, attrs, fn, skip); err != nil {
			return err
		}
	}
	return nil
}

func plainErr(err error) string {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err.Error()
	}
	return err.Error()
}
