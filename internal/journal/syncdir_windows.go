package journal

// syncDir is a no-op on Windows: NTFS journals the rename itself, and a
// directory cannot be opened for FlushFileBuffers without backup rights.
func syncDir(string) error { return nil }
