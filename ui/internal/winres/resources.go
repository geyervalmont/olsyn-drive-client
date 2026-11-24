package winres

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image/png"
	"strconv"
	"strings"
	"unicode/utf16"
)

// IconImage is one size of the icon as a PNG (Windows Vista and later read
// PNG images in icon resources).
type IconImage struct {
	PNG []byte
}

// IconGroupID is the group icon resource ID. Wails loads the window icon from
// resource 3, and Explorer shows the first group icon, which with a single
// group is this one.
const IconGroupID = 3

// Icon returns the RT_ICON resources (IDs 1..n) and the RT_GROUP_ICON that
// lists them.
func Icon(images []IconImage) ([]Resource, error) {
	if len(images) == 0 {
		return nil, fmt.Errorf("an icon needs at least one image")
	}
	var group bytes.Buffer
	le := binary.LittleEndian
	group.Write(le.AppendUint16(nil, 0)) // reserved
	group.Write(le.AppendUint16(nil, 1)) // type: icon
	group.Write(le.AppendUint16(nil, uint16(len(images))))
	out := make([]Resource, 0, len(images)+1)
	for i, img := range images {
		cfg, err := png.DecodeConfig(bytes.NewReader(img.PNG))
		if err != nil {
			return nil, fmt.Errorf("icon image %d: %w", i, err)
		}
		if cfg.Width != cfg.Height || cfg.Width < 1 || cfg.Width > 256 {
			return nil, fmt.Errorf("icon image %d is %dx%d; want square, at most 256", i, cfg.Width, cfg.Height)
		}
		side := byte(cfg.Width) // 256 is written as 0
		id := uint16(i + 1)
		group.Write([]byte{side, side, 0, 0})
		group.Write(le.AppendUint16(nil, 1))  // planes
		group.Write(le.AppendUint16(nil, 32)) // bits per pixel
		group.Write(le.AppendUint32(nil, uint32(len(img.PNG))))
		group.Write(le.AppendUint16(nil, id))
		out = append(out, Resource{Type: TypeIcon, ID: id, Data: img.PNG})
	}
	return append(out, Resource{Type: TypeGroupIcon, ID: IconGroupID, Data: group.Bytes()}), nil
}

// ICO returns the same images as a .ico file (for installers and shortcuts).
func ICO(images []IconImage) ([]byte, error) {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.Write(le.AppendUint16(nil, 0))
	b.Write(le.AppendUint16(nil, 1))
	b.Write(le.AppendUint16(nil, uint16(len(images))))
	offset := 6 + 16*len(images)
	for i, img := range images {
		cfg, err := png.DecodeConfig(bytes.NewReader(img.PNG))
		if err != nil {
			return nil, fmt.Errorf("icon image %d: %w", i, err)
		}
		side := byte(cfg.Width)
		b.Write([]byte{side, side, 0, 0})
		b.Write(le.AppendUint16(nil, 1))
		b.Write(le.AppendUint16(nil, 32))
		b.Write(le.AppendUint32(nil, uint32(len(img.PNG))))
		b.Write(le.AppendUint32(nil, uint32(offset)))
		offset += len(img.PNG)
	}
	for _, img := range images {
		b.Write(img.PNG)
	}
	return b.Bytes(), nil
}

// Manifest is the application manifest: Windows 10 and 11, per-monitor
// (v2) DPI awareness, long paths, run as the invoking user, and Common
// Controls 6 (task dialogs need it).
const Manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <description>Olsyn Drive</description>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2, PerMonitor</dpiAwareness>
      <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
    </windowsSettings>
  </application>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
</assembly>
`

// ManifestResource is the manifest as resource 1 (CREATEPROCESS_MANIFEST_RESOURCE_ID).
func ManifestResource() Resource {
	return Resource{Type: TypeManifest, ID: 1, Data: []byte(Manifest)}
}

// VersionInfo is what Explorer's Details tab shows.
type VersionInfo struct {
	// Version is the build's version string, e.g. "v1.4.2-3-gabc1234-dirty"
	// or "dev"; its leading numbers become the numeric file version.
	Version     string
	ProductName string
	Description string
	Company     string
	Copyright   string
	FileName    string
}

// OlsynDrive is the version information of olsyn-drive.exe.
func OlsynDrive(version string) VersionInfo {
	return VersionInfo{
		Version:     version,
		ProductName: "Olsyn Drive",
		Description: "Olsyn Drive",
		Company:     "Olsyn",
		Copyright:   "Copyright Olsyn. Licensed under the Apache License 2.0.",
		FileName:    "olsyn-drive.exe",
	}
}

// NumericVersion takes up to four leading numbers from a version string
// ("v1.4.2-3-g…" is 1.4.2.3 and "dev" is 0.0.0.0).
func NumericVersion(v string) [4]uint16 {
	var out [4]uint16
	v = strings.TrimPrefix(v, "v")
	i := 0
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '+' }) {
		if i == 4 {
			break
		}
		n, err := strconv.ParseUint(part, 10, 16)
		if err != nil {
			break
		}
		out[i] = uint16(n)
		i++
	}
	return out
}

// Resource returns the RT_VERSION resource (ID 1).
func (vi VersionInfo) Resource() Resource {
	n := NumericVersion(vi.Version)
	ms := uint32(n[0])<<16 | uint32(n[1])
	ls := uint32(n[2])<<16 | uint32(n[3])
	le := binary.LittleEndian
	var fixed []byte
	for _, v := range []uint32{
		0xFEEF04BD, 0x00010000, // signature, structure version
		ms, ls, ms, ls, // file and product version
		0x3F, 0, // flags mask, flags
		0x00040004, 1, 0, // VOS_NT_WINDOWS32, VFT_APP, no subtype
		0, 0, // date
	} {
		fixed = le.AppendUint32(fixed, v)
	}
	strs := [][2]string{
		{"CompanyName", vi.Company},
		{"FileDescription", vi.Description},
		{"FileVersion", vi.Version},
		{"InternalName", strings.TrimSuffix(vi.FileName, ".exe")},
		{"LegalCopyright", vi.Copyright},
		{"OriginalFilename", vi.FileName},
		{"ProductName", vi.ProductName},
		{"ProductVersion", vi.Version},
	}
	var table []node
	for _, s := range strs {
		if s[1] == "" {
			continue
		}
		val := utf16z(s[1])
		table = append(table, node{key: s[0], typ: 1, value: val, valueLen: uint16(len(val) / 2)})
	}
	root := node{key: "VS_VERSION_INFO", typ: 0, value: fixed, valueLen: uint16(len(fixed)), children: []node{
		{key: "StringFileInfo", typ: 1, children: []node{{key: "040904B0", typ: 1, children: table}}},
		{key: "VarFileInfo", typ: 1, children: []node{
			{key: "Translation", typ: 0, value: le.AppendUint32(nil, 0x04B00409), valueLen: 4},
		}},
	}}
	return Resource{Type: TypeVersion, ID: 1, Data: root.bytes()}
}

// node is one block of a VS_VERSIONINFO tree: length, value length, type,
// a NUL-terminated UTF-16 key, padding to 32 bits, the value, padding, then
// the children, each starting on a 32-bit boundary.
type node struct {
	key      string
	typ      uint16
	value    []byte
	valueLen uint16 // in bytes for binary values, in WCHARs for text
	children []node
}

func (n node) bytes() []byte {
	var b []byte
	le := binary.LittleEndian
	b = le.AppendUint16(b, 0) // length, set below
	b = le.AppendUint16(b, n.valueLen)
	b = le.AppendUint16(b, n.typ)
	b = append(b, utf16z(n.key)...)
	b = pad4(b)
	b = append(b, n.value...)
	for _, c := range n.children {
		b = pad4(b)
		b = append(b, c.bytes()...)
	}
	le.PutUint16(b, uint16(len(b)))
	return b
}

func pad4(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func utf16z(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2*len(u)+2)
	for _, c := range u {
		out = binary.LittleEndian.AppendUint16(out, c)
	}
	return append(out, 0, 0)
}
