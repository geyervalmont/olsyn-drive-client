package vfs_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// Generated files (previews, converted textures) have no versions on the
// drive, and one not built yet is listed with an estimated size. They read
// like any file: the first open builds it and gives its real size.
func TestGeneratedFilesRead(t *testing.T) {
	e := newEnv(t)
	mat := e.fd.AddFolder(e.fd.RootID(), "Oak")
	gen := e.fd.AddGeneratedFolder(mat, "revit-2048")
	built := payload(300_000, 3)
	e.fd.AddGeneratedFile(gen, "base_color.jpg", built, true, 0, 0)
	later := payload(5000, 4)
	e.fd.AddGeneratedFile(gen, "bump.png", later, false, 1234, 2) // estimate 1,234; busy twice, then built
	changing := e.fd.AddGeneratedFile(gen, "gloss.png", payload(7000, 5), true, 0, 0)
	e.start()
	defer e.stop()
	ctx := ctxT(t)

	if got, err := e.readFile("/Oak/revit-2048/base_color.jpg"); err != nil || !bytes.Equal(got, built) {
		t.Fatalf("built generated file: %d bytes, err %v", len(got), err)
	}
	// read again: from the cache, same bytes
	if got, err := e.readFile("/Oak/revit-2048/base_color.jpg"); err != nil || !bytes.Equal(got, built) {
		t.Fatalf("built generated file, second read: %d bytes, err %v", len(got), err)
	}

	if st, err := e.fs.Stat(ctx, "/Oak/revit-2048/bump.png"); err != nil || st.Size != 1234 {
		t.Fatalf("unbuilt file before it is opened: %+v, %v", st, err)
	}
	h, err := e.fs.Open(ctx, "/Oak/revit-2048/bump.png", vfs.OpenFlags{Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		t.Fatalf("open unbuilt generated file: %v", err)
	}
	if st := h.Info(); st.Size != int64(len(later)) {
		t.Fatalf("size once opened = %+v; want %d", st, len(later))
	}
	_ = h.Close()
	if got, err := e.readFile("/Oak/revit-2048/bump.png"); err != nil || !bytes.Equal(got, later) {
		t.Fatalf("generated file built on open: %d bytes, err %v", len(got), err)
	}

	// generated files are read-only
	if _, err := e.fs.Open(ctx, "/Oak/revit-2048/bump.png", vfs.OpenFlags{Access: vfs.AccessWrite, Share: vfs.ShareAll}); vfs.KindOf(err) != vfs.KindAccessDenied {
		t.Fatalf("write open of a generated file: %v", err)
	}

	// the drive rebuilt a file (its source changed) since it was listed:
	// never a mix of old size and new bytes; a later read has the new bytes
	if _, err := e.fs.Stat(ctx, "/Oak/revit-2048/gloss.png"); err != nil {
		t.Fatal(err)
	}
	rebuilt := payload(9100, 6)
	e.fd.RebuildGenerated(changing, rebuilt)
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := e.readFile("/Oak/revit-2048/gloss.png")
		if err == nil && bytes.Equal(got, rebuilt) {
			break
		}
		if err == nil {
			t.Fatalf("read %d bytes that are neither version", len(got))
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebuilt generated file never read: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
