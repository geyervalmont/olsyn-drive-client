package platform

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modmpr                 = windows.NewLazySystemDLL("mpr.dll")
	procWNetGetConnectionW = modmpr.NewProc("WNetGetConnectionW")
)

// errConnectionUnavail is ERROR_CONNECTION_UNAVAIL: a remembered network
// mapping that is not connected now.
const errConnectionUnavail = windows.Errno(1201)

// DriveLettersInUse lists the letters D: to Z: something holds: volumes,
// network drives (also remembered, disconnected mappings, which Explorer
// still lists under This PC) and subst'd folders.
func DriveLettersInUse() (map[string]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("list drives: %w", err)
	}
	remembered := rememberedMappings()
	return ClassifyLetters(LetterProbe{
		Mask: mask,
		Remote: func(l string) bool {
			root, err := windows.UTF16PtrFromString(l + "\\")
			return err == nil && windows.GetDriveType(root) == windows.DRIVE_REMOTE
		},
		Subst: isSubst,
		Remembered: func(l string) bool {
			return remembered[l] || wnetMapped(l)
		},
	}), nil
}

// wnetMapped asks the network provider about a letter: connected or
// remembered-but-unavailable both mean it is taken.
func wnetMapped(letter string) bool {
	if procWNetGetConnectionW.Find() != nil {
		return false
	}
	local, err := windows.UTF16PtrFromString(letter)
	if err != nil {
		return false
	}
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	r, _, _ := procWNetGetConnectionW.Call(uintptr(unsafe.Pointer(local)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	switch windows.Errno(r) {
	case 0, errConnectionUnavail, windows.ERROR_MORE_DATA:
		return true
	}
	return false
}

// rememberedMappings reads HKCU\Network: one subkey per persistent mapping.
func rememberedMappings() map[string]bool {
	out := map[string]bool{}
	k, err := registry.OpenKey(registry.CURRENT_USER, "Network", registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return out
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return out
	}
	for _, n := range names {
		if len(n) == 1 {
			out[strings.ToUpper(n)+":"] = true
		}
	}
	return out
}

// isSubst reports a letter that is a subst'd folder: its DOS device target
// is a path ("\??\C:\work") rather than a device.
func isSubst(letter string) bool {
	name, err := windows.UTF16PtrFromString(letter)
	if err != nil {
		return false
	}
	buf := make([]uint16, 1024)
	n, err := windows.QueryDosDevice(name, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return false
	}
	return strings.HasPrefix(windows.UTF16ToString(buf), `\??\`)
}

// FreeDiskBytes is the space available to this user on the volume holding path.
func FreeDiskBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode path: %w", err)
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, fmt.Errorf("free space of %s: %w", path, err)
	}
	return avail, nil
}
