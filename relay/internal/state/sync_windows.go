//go:build windows

package state

// syncDir: NTFS journals renames (MoveFileEx); directories can't be synced.
func syncDir(string) {}
