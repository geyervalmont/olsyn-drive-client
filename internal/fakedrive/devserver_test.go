package fakedrive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func TestVirtualFileRanges(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	id := fd.AddVirtualFile(fd.RootID(), "gen.bin", 3<<20+5)
	n, err := c.GetNode(ctx, id)
	if err != nil || n.Node.Size != 3<<20+5 {
		t.Fatalf("%+v %v", n, err)
	}
	for _, r := range [][2]int64{{0, 100}, {1001, 4097}, {3<<20 - 3, 8}} {
		d, err := c.Download(ctx, id, "", r[0], r[1])
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(d.Body)
		d.Body.Close()
		if !bytes.Equal(got, fakedrive.VirtualBytes(r[0], int(r[1]))) {
			t.Fatalf("range %v differs", r)
		}
	}
}

func TestDataDirKeepsUploadsOnDisk(t *testing.T) {
	dir := t.TempDir()
	fd := fakedrive.New(fakedrive.Options{PartSize: 64 << 10, SingleMax: 100 << 10, DataDir: dir})
	t.Cleanup(fd.Close)
	c, _ := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return fd.Token(), nil }})
	ctx := ctxT(t)
	data := pattern(200<<10, 9)
	tk, err := c.StartUpload(ctx, driveapi.StartUpload{ParentID: fd.RootID(), Name: "disk.bin", Size: int64(len(data)), Conflict: driveapi.ConflictFail})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upload(ctx, c, tk, data); err != nil {
		t.Fatal(err)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "parts", "*", "*"))
	if len(parts) != 4 {
		t.Fatalf("parts on disk: %v", parts)
	}
	fi, ok := fd.Lookup("/disk.bin")
	if !ok || !bytes.Equal(fi.Data, data) {
		t.Fatal("content differs")
	}
	d, err := c.Download(ctx, fi.ID, "", 70<<10, 10<<10)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(d.Body)
	d.Body.Close()
	if !bytes.Equal(got, data[70<<10:80<<10]) {
		t.Fatal("range across parts differs")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLinkPageApproves(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	ls, err := c.StartLink(ctx, driveapi.LinkRequest{Client: "olsyn-drive-test", Machine: "Test PC", AppVersion: "0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ls.VerificationURL, fd.URL()+"/link?code=") {
		t.Fatalf("verification url %q", ls.VerificationURL)
	}
	page := get(t, ls.VerificationURL)
	if !strings.Contains(page, ls.UserCode) || !strings.Contains(page, "Approve") {
		t.Fatalf("page: %s", page)
	}
	if strings.Contains(get(t, fd.URL()+"/link?code=NOPE-NOPE"), "Approve") {
		t.Fatal("unknown code offered approval")
	}
	res, err := http.PostForm(fd.URL()+"/link", url.Values{"code": {ls.UserCode}, "decision": {"approve"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got, err := c.WaitLink(ctx, ls); err != nil || got.Token == "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestAutoApproveAndManaged(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{AutoApprove: true})
	t.Cleanup(fd.Close)
	c, _ := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return fd.Token(), nil }})
	ctx := ctxT(t)
	ls, err := c.StartLink(ctx, driveapi.LinkRequest{Client: "olsyn-drive-test", Machine: "Test PC", AppVersion: "0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitLink(ctx, ls); err != nil {
		t.Fatal(err)
	}
	m := fd.AddFolder(fd.RootID(), "Managed")
	f := fd.AddFile(m, "out.txt", []byte("x"))
	fd.RefuseWrites(m, driveapi.CodeManaged)
	n, err := c.GetNode(ctx, f)
	if err != nil || !n.Node.IsManaged() || n.Node.Can.Write {
		t.Fatalf("%+v %v", n, err)
	}
	_, err = c.StartUpload(ctx, driveapi.StartUpload{ParentID: m, Name: "new.txt", Size: 1, Conflict: driveapi.ConflictFail})
	var ae *driveapi.Error
	if !errors.As(err, &ae) || ae.Code != driveapi.CodeManaged {
		t.Fatalf("upload into managed: %v", err)
	}
	st := fd.Stats()
	if st.Files != 1 || st.Folders != 1 || st.SignedInDevices != 1 {
		t.Fatalf("%+v", st)
	}
}

func get(t *testing.T, u string) string {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
