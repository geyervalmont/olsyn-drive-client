package winres

import (
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Program is what a built Windows program says about itself: what a person
// (or a malware analyst) sees in Explorer's Details tab, and the facts a
// false-positive report asks for.
type Program struct {
	FileName string
	Size     int64
	SHA256   string
	// FileVersion is the numeric file version from the fixed part of the
	// version resource.
	FileVersion [4]uint16
	// Strings are the version resource's text entries (CompanyName,
	// ProductName, FileVersion, ...).
	Strings map[string]string
	// ExecutionLevel is requestedExecutionLevel in the manifest
	// ("asInvoker" runs without asking for administrator rights).
	ExecutionLevel string
}

// RequiredStrings are the version entries every release must carry.
var RequiredStrings = []string{"CompanyName", "ProductName", "FileDescription", "FileVersion",
	"ProductVersion", "LegalCopyright", "OriginalFilename"}

// ReadProgram reads a Windows program's size, SHA-256, version resource and
// manifest. It works on any OS.
func ReadProgram(path string) (Program, error) {
	p := Program{FileName: filepath.Base(path)}
	f, err := os.Open(path)
	if err != nil {
		return p, fmt.Errorf("open program: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return p, fmt.Errorf("read %s: %w", p.FileName, err)
	}
	p.Size, p.SHA256 = n, hex.EncodeToString(h.Sum(nil))

	img, err := pe.NewFile(f)
	if err != nil {
		return p, fmt.Errorf("%s is not a Windows program: %w", p.FileName, err)
	}
	defer img.Close()
	sec := img.Section(".rsrc")
	if sec == nil {
		return p, fmt.Errorf("%s has no resources", p.FileName)
	}
	data, err := sec.Data()
	if err != nil {
		return p, fmt.Errorf("read resources: %w", err)
	}
	r := rsrc{data: data, rva: sec.VirtualAddress}
	ver, err := r.first(TypeVersion)
	if err != nil {
		return p, fmt.Errorf("version information: %w", err)
	}
	if p.FileVersion, p.Strings, err = parseVersion(ver); err != nil {
		return p, fmt.Errorf("version information: %w", err)
	}
	manifest, err := r.first(TypeManifest)
	if err != nil {
		return p, fmt.Errorf("manifest: %w", err)
	}
	p.ExecutionLevel = executionLevel(string(manifest))
	return p, nil
}

// Missing lists the required version entries that are absent or empty.
func (p Program) Missing() []string {
	var out []string
	for _, k := range RequiredStrings {
		if strings.TrimSpace(p.Strings[k]) == "" {
			out = append(out, k)
		}
	}
	return out
}

var errResource = errors.New("malformed resource section")

// rsrc is a resource section as it sits in an image whose section starts at rva.
type rsrc struct {
	data []byte
	rva  uint32
}

func (r rsrc) u16(at uint32) (uint16, error) {
	if uint64(at)+2 > uint64(len(r.data)) {
		return 0, errResource
	}
	return binary.LittleEndian.Uint16(r.data[at:]), nil
}

func (r rsrc) u32(at uint32) (uint32, error) {
	if uint64(at)+4 > uint64(len(r.data)) {
		return 0, errResource
	}
	return binary.LittleEndian.Uint32(r.data[at:]), nil
}

// entries returns a directory's (name or id, offset) pairs.
func (r rsrc) entries(at uint32) ([][2]uint32, error) {
	named, err := r.u16(at + 12)
	if err != nil {
		return nil, err
	}
	ids, err := r.u16(at + 14)
	if err != nil {
		return nil, err
	}
	var out [][2]uint32
	for i := uint32(0); i < uint32(named)+uint32(ids); i++ {
		name, err := r.u32(at + 16 + 8*i)
		if err != nil {
			return nil, err
		}
		off, err := r.u32(at + 20 + 8*i)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]uint32{name, off})
	}
	return out, nil
}

// first returns the data of the first resource of a type (any id, any language).
func (r rsrc) first(typ uint16) ([]byte, error) {
	types, err := r.entries(0)
	if err != nil {
		return nil, err
	}
	for _, te := range types {
		if te[0] != uint32(typ) {
			continue
		}
		at := te[1]
		// Down the id and language levels to the first data entry.
		for at&0x80000000 != 0 {
			es, err := r.entries(at &^ 0x80000000)
			if err != nil {
				return nil, err
			}
			if len(es) == 0 {
				return nil, errResource
			}
			at = es[0][1]
		}
		dataRVA, err := r.u32(at)
		if err != nil {
			return nil, err
		}
		size, err := r.u32(at + 4)
		if err != nil {
			return nil, err
		}
		if dataRVA < r.rva || uint64(dataRVA-r.rva)+uint64(size) > uint64(len(r.data)) {
			return nil, errResource
		}
		return r.data[dataRVA-r.rva : dataRVA-r.rva+size], nil
	}
	return nil, errors.New("not present")
}

// parseVersion reads a VS_VERSIONINFO block: the numeric file version and
// every string under StringFileInfo.
func parseVersion(b []byte) ([4]uint16, map[string]string, error) {
	var fv [4]uint16
	strs := map[string]string{}
	root, err := parseNode(b, 0)
	if err != nil {
		return fv, nil, err
	}
	if root.key != "VS_VERSION_INFO" || len(root.value) < 16 ||
		binary.LittleEndian.Uint32(root.value) != 0xFEEF04BD {
		return fv, nil, errors.New("no VS_VERSION_INFO")
	}
	ms := binary.LittleEndian.Uint32(root.value[8:])
	ls := binary.LittleEndian.Uint32(root.value[12:])
	fv = [4]uint16{uint16(ms >> 16), uint16(ms), uint16(ls >> 16), uint16(ls)}
	for _, c := range root.children {
		if c.key != "StringFileInfo" {
			continue
		}
		for _, table := range c.children {
			for _, s := range table.children {
				strs[s.key] = s.text()
			}
		}
	}
	return fv, strs, nil
}

type parsed struct {
	key      string
	typ      uint16
	value    []byte
	children []parsed
}

func (p parsed) text() string {
	u := make([]uint16, 0, len(p.value)/2)
	for i := 0; i+1 < len(p.value); i += 2 {
		c := binary.LittleEndian.Uint16(p.value[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func align4(n int) int { return (n + 3) &^ 3 }

// parseNode reads the block at off; see node for the layout.
func parseNode(b []byte, off int) (parsed, error) {
	var p parsed
	if off+6 > len(b) {
		return p, errResource
	}
	length := int(binary.LittleEndian.Uint16(b[off:]))
	valueLen := int(binary.LittleEndian.Uint16(b[off+2:]))
	p.typ = binary.LittleEndian.Uint16(b[off+4:])
	end := off + length
	if length < 6 || end > len(b) {
		return p, errResource
	}
	at := off + 6
	var key []uint16
	for {
		if at+2 > end {
			return p, errResource
		}
		c := binary.LittleEndian.Uint16(b[at:])
		at += 2
		if c == 0 {
			break
		}
		key = append(key, c)
	}
	p.key = string(utf16.Decode(key))
	at = align4(at)
	if p.typ == 1 {
		valueLen *= 2 // text values count WCHARs
	}
	if at+valueLen > end {
		return p, errResource
	}
	p.value = b[at : at+valueLen]
	at = align4(at + valueLen)
	for at < end {
		c, err := parseNode(b, at)
		if err != nil {
			return p, err
		}
		p.children = append(p.children, c)
		l := int(binary.LittleEndian.Uint16(b[at:]))
		at = align4(at + l)
	}
	return p, nil
}

// executionLevel finds requestedExecutionLevel's level in a manifest.
func executionLevel(manifest string) string {
	i := strings.Index(manifest, "requestedExecutionLevel")
	if i < 0 {
		return ""
	}
	rest := manifest[i:]
	j := strings.Index(rest, `level="`)
	if j < 0 {
		return ""
	}
	rest = rest[j+len(`level="`):]
	if k := strings.IndexByte(rest, '"'); k >= 0 {
		return rest[:k]
	}
	return ""
}
