package vfs

import (
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// Names on the drive may hold characters Windows forbids (< > : " | ? *),
// end in a dot or space, or be a reserved device name (CON, NUL, COM1, …).
// They are shown with a reversible substitution: each forbidden character
// becomes a look-alike from the full-width block, a trailing dot or space
// becomes '．' or '␠', and a reserved name gets its last letter full-width.
// A drive name that already contains one of the substitutes is shown with
// the quote '‛' before it, so decoding is exact. The same mapping is used on
// every OS so a name looks the same everywhere.

const quote = '‛'

var toLocal = map[rune]rune{
	'<': '＜', '>': '＞', ':': '：', '"': '＂', '|': '｜', '?': '？', '*': '＊',
}

var fromLocal = func() map[rune]rune {
	m := map[rune]rune{'．': '.', '␠': ' '}
	for k, v := range toLocal {
		m[v] = k
	}
	for c := 'A'; c <= 'Z'; c++ {
		m[c-'A'+'Ａ'] = c
	}
	for c := 'a'; c <= 'z'; c++ {
		m[c-'a'+'ａ'] = c
	}
	return m
}()

func isReserved(base string) bool {
	u := strings.ToUpper(base)
	switch u {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(u) == 4 && (strings.HasPrefix(u, "COM") || strings.HasPrefix(u, "LPT")) && u[3] >= '0' && u[3] <= '9' {
		return true
	}
	return false
}

// EncodeName maps a drive name to the name shown by the OS.
func EncodeName(name string) string {
	var b strings.Builder
	rs := []rune(name)
	// a reserved device name: make its last letter full-width
	stem := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		stem = name[:i]
	}
	reservedAt := -1
	if isReserved(strings.TrimRight(stem, " ")) {
		reservedAt = utf8.RuneCountInString(strings.TrimRight(stem, " ")) - 1
		for reservedAt >= 0 && !isASCIILetter(rs[reservedAt]) {
			reservedAt--
		}
	}
	for i, r := range rs {
		last := i == len(rs)-1
		switch {
		case r == quote:
			b.WriteRune(quote)
			b.WriteRune(r)
		case fromLocal[r] != 0:
			b.WriteRune(quote)
			b.WriteRune(r)
		case toLocal[r] != 0:
			b.WriteRune(toLocal[r])
		case last && r == '.':
			b.WriteRune('．')
		case last && r == ' ':
			b.WriteRune('␠')
		case i == reservedAt:
			if r >= 'a' {
				b.WriteRune(r - 'a' + 'ａ')
			} else {
				b.WriteRune(r - 'A' + 'Ａ')
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// DecodeName maps a name from the OS back to the drive name.
func DecodeName(local string) string {
	var b strings.Builder
	rs := []rune(local)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == quote && i+1 < len(rs) {
			b.WriteRune(rs[i+1])
			i++
			continue
		}
		if o, ok := fromLocal[r]; ok {
			b.WriteRune(o)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validName reports a drive name the server accepts.
func validName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(name)
}

// IsJunk reports files the OS or apps make that stay on this PC and are
// never uploaded: Thumbs.db, desktop.ini, .DS_Store, AppleDouble "._x",
// Office owner files "~$x", Office temp files "~x.tmp" and LibreOffice
// lock files ".~lock.x#".
func IsJunk(name string) bool {
	l := strings.ToLower(name)
	switch l {
	case "thumbs.db", "ehthumbs.db", "desktop.ini", ".ds_store", "icon\r":
		return true
	}
	return strings.HasPrefix(l, "~$") || strings.HasPrefix(l, "._") ||
		(strings.HasPrefix(l, "~") && strings.HasSuffix(l, ".tmp")) ||
		(strings.HasPrefix(l, ".~lock.") && strings.HasSuffix(l, "#"))
}

// ConflictName is the name of a conflict copy:
// "name (conflict from <PC>, 2026-11-24 1530).ext".
func ConflictName(name, pc string, at time.Time) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	pc = strings.Map(func(r rune) rune {
		if strings.ContainsRune("/\\<>:\"|?*", r) || r < 0x20 {
			return '-'
		}
		return r
	}, pc)
	return stem + " (conflict from " + pc + ", " + at.Local().Format("2006-01-02 1504") + ")" + ext
}

// splitPath splits "/a/b/c" (or "\a\b\c") into OS names.
func splitPath(p string) []string {
	p = strings.ReplaceAll(p, "\\", "/")
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}
