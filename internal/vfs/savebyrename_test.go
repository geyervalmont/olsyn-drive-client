package vfs_test

import (
	"slices"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// A temp file renamed over a drive file is a new version of it.
func TestSaveByRenameOverIsANewVersion(t *testing.T) {
	e := newEnv(t)
	id := e.fd.AddFile(e.fd.RootID(), "plan.txt", []byte("first"))
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	if _, err := e.readFile("/plan.txt"); err != nil {
		t.Fatal(err)
	}
	if err := e.writeFile("/~WRL0001.tmp", []byte("second"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, "/~WRL0001.tmp", "/plan.txt", true); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(10 * time.Second)
	fi, ok := e.fd.Lookup("/plan.txt")
	if !ok || fi.ID != id || fi.Versions != 2 || string(fi.Data) != "second" {
		t.Fatalf("on the drive: ok=%v id=%s (want %s) versions=%d data=%q", ok, fi.ID, id, fi.Versions, fi.Data)
	}
	if got := e.fd.List(e.fd.RootID()); !slices.Equal(got, []string{"plan.txt"}) || e.fd.Count("trash") != 0 {
		t.Fatalf("drive root %v, %d trashed", got, e.fd.Count("trash"))
	}
}

// Word's save while another program keeps the document open: the original
// is renamed to a temp name, the new copy takes its name, the original is
// deleted. The drive sees one new version.
func TestWordStyleSaveIsANewVersion(t *testing.T) {
	e := newEnv(t)
	id := e.fd.AddFile(e.fd.RootID(), "report.docx", []byte("draft one"))
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	other, err := e.fs.Open(ctx, "/report.docx", vfs.OpenFlags{Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.writeFile("/~WRL0002.tmp", []byte("draft two"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, "/report.docx", "/~WRL0003.tmp", false); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, "/~WRL0002.tmp", "/report.docx", false); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(ctx, "/~WRL0003.tmp"); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(10 * time.Second)
	fi, ok := e.fd.Lookup("/report.docx")
	if !ok || fi.ID != id || fi.Versions != 2 || string(fi.Data) != "draft two" {
		t.Fatalf("on the drive: ok=%v id=%s (want %s) versions=%d data=%q", ok, fi.ID, id, fi.Versions, fi.Data)
	}
	if got := e.fd.List(e.fd.RootID()); !slices.Equal(got, []string{"report.docx"}) || e.fd.Count("trash") != 0 {
		t.Fatalf("drive root %v, %d trashed", got, e.fd.Count("trash"))
	}
	if got, err := e.readFile("/report.docx"); err != nil || string(got) != "draft two" {
		t.Fatalf("on this PC: %q %v", got, err)
	}
}
