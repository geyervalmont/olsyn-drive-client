// Package winres writes a Windows resource object (.syso) for the Go linker:
// the application icon, the application manifest and the version
// information, with no cgo, no Windows tools and no third-party module.
//
// The output is a COFF object with one .rsrc section holding a resource
// directory (type, name, language) and the data, plus one relocation per data
// entry (its OffsetToData is an address relative to the image base, which the
// linker fills in). cmd/link reads such objects from .syso files in a
// package's folder.
package winres

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
)

// Resource types and the language used for every resource (en-US).
const (
	TypeIcon      = 3
	TypeGroupIcon = 14
	TypeVersion   = 16
	TypeManifest  = 24

	LangEnUS = 0x0409
)

// Machine names a target for the object header and its relocation type.
type Machine struct {
	ID        uint16 // IMAGE_FILE_MACHINE_*
	RelocType uint16 // the ADDR32NB (image-relative 32-bit address) relocation
}

var machines = map[string]Machine{
	"386":   {ID: 0x014c, RelocType: 0x0007}, // IMAGE_REL_I386_DIR32NB
	"amd64": {ID: 0x8664, RelocType: 0x0003}, // IMAGE_REL_AMD64_ADDR32NB
	"arm64": {ID: 0xaa64, RelocType: 0x0002}, // IMAGE_REL_ARM64_ADDR32NB
}

// MachineFor returns the machine for a GOARCH.
func MachineFor(goarch string) (Machine, error) {
	m, ok := machines[goarch]
	if !ok {
		return Machine{}, fmt.Errorf("no Windows resource support for GOARCH %q", goarch)
	}
	return m, nil
}

// Resource is one resource: its type, numeric ID and data.
type Resource struct {
	Type uint16
	ID   uint16
	Data []byte
}

// Object returns the COFF object holding the resources for machine m.
// Resources are sorted by type then ID, as the directory requires.
func Object(m Machine, resources []Resource) ([]byte, error) {
	if len(resources) == 0 {
		return nil, fmt.Errorf("no resources")
	}
	res := slices.Clone(resources)
	slices.SortFunc(res, func(a, b Resource) int {
		if a.Type != b.Type {
			return int(a.Type) - int(b.Type)
		}
		return int(a.ID) - int(b.ID)
	})
	for i := 1; i < len(res); i++ {
		if res[i].Type == res[i-1].Type && res[i].ID == res[i-1].ID {
			return nil, fmt.Errorf("resource type %d id %d given twice", res[i].Type, res[i].ID)
		}
	}
	section, relocs := directory(res)

	const fileHeader, sectionHeader = 20, 40
	dataAt := fileHeader + sectionHeader
	relocAt := dataAt + len(section)
	symAt := relocAt + 10*len(relocs)

	var b bytes.Buffer
	le := binary.LittleEndian
	w16 := func(v uint16) { b.Write(le.AppendUint16(nil, v)) }
	w32 := func(v uint32) { b.Write(le.AppendUint32(nil, v)) }
	// IMAGE_FILE_HEADER
	w16(m.ID)
	w16(1)             // sections
	w32(0)             // time stamp: 0 keeps builds reproducible
	w32(uint32(symAt)) // symbol table
	w32(1)             // symbols
	w16(0)             // no optional header in an object
	w16(0)             // characteristics
	// IMAGE_SECTION_HEADER
	b.WriteString(".rsrc\x00\x00\x00")
	w32(0) // virtual size
	w32(0) // virtual address
	w32(uint32(len(section)))
	w32(uint32(dataAt))
	w32(uint32(relocAt))
	w32(0) // line numbers
	w16(uint16(len(relocs)))
	w16(0)
	w32(0x40000040) // initialised data, readable
	b.Write(section)
	for _, at := range relocs {
		w32(at) // where the address goes, from the start of the section
		w32(0)  // symbol 0: the section itself
		w16(m.RelocType)
	}
	// The section symbol the relocations refer to.
	b.WriteString(".rsrc\x00\x00\x00")
	w32(0)         // value
	w16(1)         // section number
	w16(0)         // type
	b.WriteByte(3) // IMAGE_SYM_CLASS_STATIC
	b.WriteByte(0) // no auxiliary records
	w32(4)         // empty string table: just its own size
	if len(relocs) > 0xffff {
		return nil, fmt.Errorf("too many resources")
	}
	return b.Bytes(), nil
}

// directory lays out the resource section: the type directory, one name
// directory per type, one language directory per name, the data entries,
// then the data (each blob 8-byte aligned). It returns the section and the
// offsets of the OffsetToData fields to relocate.
func directory(res []Resource) ([]byte, []uint32) {
	type typeGroup struct {
		typ uint16
		res []Resource
	}
	var groups []typeGroup
	for _, r := range res {
		if len(groups) == 0 || groups[len(groups)-1].typ != r.Type {
			groups = append(groups, typeGroup{typ: r.Type})
		}
		groups[len(groups)-1].res = append(groups[len(groups)-1].res, r)
	}
	dirSize := func(entries int) int { return 16 + 8*entries }

	// Offsets of everything, in layout order.
	off := dirSize(len(groups))
	nameDirAt := make([]int, len(groups))
	for i, g := range groups {
		nameDirAt[i] = off
		off += dirSize(len(g.res))
	}
	langDirAt := make([]int, len(res))
	for i := range res {
		langDirAt[i] = off
		off += dirSize(1)
	}
	entryAt := make([]int, len(res))
	for i := range res {
		entryAt[i] = off
		off += 16
	}
	dataAt := make([]int, len(res))
	for i, r := range res {
		off = align(off, 8)
		dataAt[i] = off
		off += len(r.Data)
	}
	buf := make([]byte, align(off, 8))
	le := binary.LittleEndian
	putDir := func(at, ids int) {
		// Characteristics, time stamp, version: zero. Named entries: none.
		le.PutUint16(buf[at+14:], uint16(ids))
	}
	putEntry := func(at int, id uint32, target int, sub bool) {
		le.PutUint32(buf[at:], id)
		v := uint32(target)
		if sub {
			v |= 0x80000000
		}
		le.PutUint32(buf[at+4:], v)
	}

	putDir(0, len(groups))
	k := 0
	for i, g := range groups {
		putEntry(16+8*i, uint32(g.typ), nameDirAt[i], true)
		putDir(nameDirAt[i], len(g.res))
		for j, r := range g.res {
			putEntry(nameDirAt[i]+16+8*j, uint32(r.ID), langDirAt[k], true)
			putDir(langDirAt[k], 1)
			putEntry(langDirAt[k]+16, LangEnUS, entryAt[k], false)
			k++
		}
	}
	relocs := make([]uint32, len(res))
	for i, r := range res {
		// IMAGE_RESOURCE_DATA_ENTRY: OffsetToData is image-relative, so it
		// holds the offset in the section and is relocated by the linker.
		le.PutUint32(buf[entryAt[i]:], uint32(dataAt[i]))
		le.PutUint32(buf[entryAt[i]+4:], uint32(len(r.Data)))
		relocs[i] = uint32(entryAt[i])
		copy(buf[dataAt[i]:], r.Data)
	}
	return buf, relocs
}

func align(n, to int) int { return (n + to - 1) / to * to }
