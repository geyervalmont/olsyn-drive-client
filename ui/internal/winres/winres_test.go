package winres

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"encoding/xml"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func testPNG(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	img.Set(0, 0, color.NRGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testResources(t *testing.T) []Resource {
	t.Helper()
	icon, err := Icon([]IconImage{{PNG: testPNG(t, 16)}, {PNG: testPNG(t, 32)}, {PNG: testPNG(t, 256)}})
	if err != nil {
		t.Fatal(err)
	}
	return append(icon, ManifestResource(), VersionInfo{Version: "v1.4.2-3-gabc", ProductName: "Olsyn Drive", FileName: "olsyn-drive.exe"}.Resource())
}

// walk reads a resource section (as it sits in an object, or in an image
// whose section starts at rva) into type -> id -> data.
func walk(t *testing.T, sec []byte, rva uint32) map[uint16]map[uint16][]byte {
	t.Helper()
	le := binary.LittleEndian
	entries := func(at uint32) [][2]uint32 {
		n := uint32(le.Uint16(sec[at+12:])) + uint32(le.Uint16(sec[at+14:]))
		var out [][2]uint32
		for i := uint32(0); i < n; i++ {
			e := at + 16 + 8*i
			out = append(out, [2]uint32{le.Uint32(sec[e:]), le.Uint32(sec[e+4:])})
		}
		return out
	}
	out := map[uint16]map[uint16][]byte{}
	for _, te := range entries(0) {
		if te[1]&0x80000000 == 0 {
			t.Fatal("type entry is not a directory")
		}
		out[uint16(te[0])] = map[uint16][]byte{}
		for _, ne := range entries(te[1] &^ 0x80000000) {
			langs := entries(ne[1] &^ 0x80000000)
			if len(langs) != 1 || langs[0][0] != LangEnUS || langs[0][1]&0x80000000 != 0 {
				t.Fatalf("language entries %v", langs)
			}
			de := langs[0][1]
			off, size := le.Uint32(sec[de:])-rva, le.Uint32(sec[de+4:])
			out[uint16(te[0])][uint16(ne[0])] = sec[off : off+size]
		}
	}
	return out
}

func checkResources(t *testing.T, got map[uint16]map[uint16][]byte) {
	t.Helper()
	if len(got[TypeIcon]) != 3 || got[TypeGroupIcon][IconGroupID] == nil || got[TypeManifest][1] == nil || got[TypeVersion][1] == nil {
		t.Fatalf("resource types: %v", keys(got))
	}
	group := got[TypeGroupIcon][IconGroupID]
	le := binary.LittleEndian
	if le.Uint16(group[2:]) != 1 || le.Uint16(group[4:]) != 3 {
		t.Fatalf("group header % x", group[:6])
	}
	for i, side := range []byte{16, 32, 0} {
		e := group[6+14*i:]
		id := le.Uint16(e[12:])
		if e[0] != side || int(le.Uint32(e[8:])) != len(got[TypeIcon][id]) {
			t.Fatalf("group entry %d: % x", i, e[:14])
		}
		if !bytes.HasPrefix(got[TypeIcon][id], []byte("\x89PNG")) {
			t.Fatalf("icon %d is not a PNG", id)
		}
	}
	if err := xml.Unmarshal(got[TypeManifest][1], new(struct{})); err != nil {
		t.Fatalf("manifest is not XML: %v", err)
	}
	ver := got[TypeVersion][1]
	if int(le.Uint16(ver)) != len(ver) {
		t.Fatalf("version length %d, data %d", le.Uint16(ver), len(ver))
	}
	text := decodeUTF16(ver)
	for _, want := range []string{"VS_VERSION_INFO", "StringFileInfo", "040904B0", "ProductName", "Olsyn Drive", "v1.4.2-3-gabc", "VarFileInfo", "Translation"} {
		if !strings.Contains(text, want) {
			t.Errorf("version info lacks %q", want)
		}
	}
	fixed := bytes.Index(ver, le.AppendUint32(nil, 0xFEEF04BD))
	if fixed < 0 || fixed%4 != 0 {
		t.Fatalf("VS_FIXEDFILEINFO at %d", fixed)
	}
	if ms, ls := le.Uint32(ver[fixed+8:]), le.Uint32(ver[fixed+12:]); ms != 1<<16|4 || ls != 2<<16|3 {
		t.Fatalf("file version %08x %08x", ms, ls)
	}
}

func keys(m map[uint16]map[uint16][]byte) []uint16 {
	var out []uint16
	for k := range m {
		out = append(out, k)
	}
	return out
}

func decodeUTF16(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

func TestObject(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64", "386"} {
		m, err := MachineFor(arch)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := Object(m, testResources(t))
		if err != nil {
			t.Fatal(err)
		}
		f, err := pe.NewFile(bytes.NewReader(obj))
		if err != nil {
			t.Fatalf("%s: debug/pe cannot read the object: %v", arch, err)
		}
		if f.Machine != m.ID || len(f.Sections) != 1 || f.Sections[0].Name != ".rsrc" || len(f.Symbols) != 1 {
			t.Fatalf("%s: machine %x sections %v symbols %v", arch, f.Machine, f.Sections, f.Symbols)
		}
		sec := f.Sections[0]
		if len(sec.Relocs) != 6 {
			t.Fatalf("%s: %d relocations, want one per resource (6)", arch, len(sec.Relocs))
		}
		for _, r := range sec.Relocs {
			if r.Type != m.RelocType || r.SymbolTableIndex != 0 {
				t.Fatalf("%s: relocation %+v", arch, r)
			}
		}
		data, err := sec.Data()
		if err != nil {
			t.Fatal(err)
		}
		checkResources(t, walk(t, data, 0))
	}
	if _, err := MachineFor("riscv64"); err == nil {
		t.Fatal("riscv64 accepted")
	}
	if _, err := Object(machines["amd64"], []Resource{{Type: 24, ID: 1}, {Type: 24, ID: 1}}); err == nil {
		t.Fatal("a duplicate resource was accepted")
	}
}

func TestNumericVersion(t *testing.T) {
	for in, want := range map[string][4]uint16{
		"v1.4.2":                  {1, 4, 2, 0},
		"v1.4.2-3-gabc1234-dirty": {1, 4, 2, 3},
		"1.2.3.4.5":               {1, 2, 3, 4},
		"dev":                     {},
		"abc1234-dev":             {},
	} {
		if got := NumericVersion(in); got != want {
			t.Errorf("NumericVersion(%q) = %v, want %v", in, got, want)
		}
	}
}

// The real check: cmd/link takes the object and the linked program carries
// the resources at the right addresses.
func TestLinkedProgramCarriesTheResources(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a Windows program")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			obj, err := Object(machines[arch], testResources(t))
			if err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				"go.mod":  "module wrtest\n\ngo 1.21\n",
				"main.go": "package main\n\nfunc main() {}\n",
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "rsrc_windows_"+arch+".syso"), obj, 0o644); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(dir, "t.exe")
			cmd := exec.Command(goBin, "build", "-o", exe, ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go build: %v\n%s", err, out)
			}
			f, err := pe.Open(exe)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			sec := f.Section(".rsrc")
			if sec == nil {
				t.Fatal("the program has no .rsrc section")
			}
			data, err := sec.Data()
			if err != nil {
				t.Fatal(err)
			}
			var dirRVA uint32
			switch oh := f.OptionalHeader.(type) {
			case *pe.OptionalHeader64:
				dirRVA = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
			case *pe.OptionalHeader32:
				dirRVA = oh.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_RESOURCE].VirtualAddress
			}
			if dirRVA != sec.VirtualAddress {
				t.Fatalf("resource directory at %x, .rsrc at %x", dirRVA, sec.VirtualAddress)
			}
			checkResources(t, walk(t, data, sec.VirtualAddress))
		})
	}
}
