package platform

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FreeDriveLetters lists unused letters D: to Z:, last first (W: style letters
// near the end are the usual choice for network drives).
func FreeDriveLetters() ([]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("list drives: %w", err)
	}
	var free []string
	for i := 25; i >= 3; i-- {
		if mask&(1<<uint(i)) == 0 {
			free = append(free, string(rune('A'+i))+":")
		}
	}
	return free, nil
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
