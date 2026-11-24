package fakedrive_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func setup(t *testing.T) (*fakedrive.Server, *driveapi.Client) {
	t.Helper()
	fd := fakedrive.New(fakedrive.Options{PartSize: 64 << 10, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	c, err := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return fd.Token(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return fd, c
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) + seed
	}
	return b
}

// upload runs a whole multipart upload the simple way.
func upload(ctx context.Context, c *driveapi.Client, tk *driveapi.UploadTicket, data []byte) (*driveapi.Node, error) {
	if tk.Method == driveapi.MethodSingle {
		if _, err := c.PutPart(ctx, tk.URL, tk.Headers, bytes.NewReader(data), int64(len(data))); err != nil && !driveapi.IsAlreadyStored(err) {
			return nil, err
		}
		return c.Complete(ctx, tk.UploadID, driveapi.CompleteUpload{})
	}
	for _, p := range tk.Parts {
		off := int64(p.Number-1) * tk.PartSize
		end := min(off+tk.PartSize, int64(len(data)))
		if _, err := c.PutPart(ctx, p.URL, nil, bytes.NewReader(data[off:end]), end-off); err != nil {
			return nil, err
		}
	}
	return c.Complete(ctx, tk.UploadID, driveapi.CompleteUpload{})
}

func TestLinkFlow(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	ls, err := c.StartLink(ctx, driveapi.LinkRequest{Client: "olsyn-drive-test", Machine: "Test PC", AppVersion: "0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLink(ctx, ls.DeviceCode); !driveapi.IsLinkPending(err) {
		t.Fatalf("want pending, got %v", err)
	}
	fd.ApproveLinks()
	res, err := c.WaitLink(ctx, ls)
	if err != nil || !strings.HasPrefix(res.Token, "odt_") {
		t.Fatalf("res %+v err %v", res, err)
	}
	c2, _ := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return res.Token, nil }})
	if _, err := c2.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.SendHeartbeat(ctx, driveapi.Heartbeat{State: "mounted", AppVersion: "0.0.1"}); err != nil {
		t.Fatal(err)
	}
	fd.RevokeToken(res.Token)
	if err := c2.SendHeartbeat(ctx, driveapi.Heartbeat{State: "mounted"}); !driveapi.IsAuthLost(err) {
		t.Fatalf("revoked token: %v", err)
	}
}

func TestChildrenPagingAndChanges(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	first, err := c.Changes(ctx, -1, 0, "")
	if err != nil || first.Reset == nil || first.Reset.Reason != driveapi.ResetNoCursor {
		t.Fatalf("bootstrap changes %+v %v", first, err)
	}
	dir := fd.AddFolder(fd.RootID(), "Big")
	for i := 0; i < 1203; i++ {
		fd.AddFile(dir, fmt.Sprintf("f%04d.txt", i), []byte("x"))
	}
	all, err := c.AllChildren(ctx, dir)
	if err != nil || len(all) != 1203 {
		t.Fatalf("children %d %v", len(all), err)
	}
	since := first.Next
	seen := map[string]bool{}
	for {
		p, err := c.Changes(ctx, since, 500, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range p.Changes {
			seen[ch.Node.ID] = true
		}
		since = p.Next
		if !p.HasMore {
			break
		}
	}
	if len(seen) != 1204 {
		t.Fatalf("saw %d nodes change", len(seen))
	}
	fd.ForceReset(driveapi.ResetAccessChanged)
	p, err := c.Changes(ctx, since, 0, "")
	if err != nil || p.Reset == nil || p.Reset.Reason != driveapi.ResetAccessChanged {
		t.Fatalf("reset %+v %v", p, err)
	}
}

func TestMultipartResumeAndDownload(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	data := pattern(300<<10, 3)
	mt := time.Date(2025, 1, 2, 3, 4, 5, 678e6, time.UTC)
	req := driveapi.StartUpload{ParentID: fd.RootID(), Name: "big.bin", Size: int64(len(data)), Conflict: driveapi.ConflictFail,
		Fingerprint: "v1|big.bin|300k", ModifiedAt: driveapi.NewTime(mt)}
	tk, err := c.StartUpload(ctx, req)
	if err != nil || tk.Method != driveapi.MethodMultipart || tk.PartCount != 5 {
		t.Fatalf("ticket %+v %v", tk, err)
	}
	// send parts 1 and 2, then "crash"
	for _, p := range tk.Parts[:2] {
		off := int64(p.Number-1) * tk.PartSize
		if _, err := c.PutPart(ctx, p.URL, nil, bytes.NewReader(data[off:off+tk.PartSize]), tk.PartSize); err != nil {
			t.Fatal(err)
		}
	}
	fd.DropNextPartPuts(1, 1000)
	p3 := tk.Parts[2]
	if _, err := c.PutPart(ctx, p3.URL, nil, bytes.NewReader(data[2*tk.PartSize:3*tk.PartSize]), tk.PartSize); !driveapi.IsRetryable(err) {
		t.Fatalf("dropped put should be retryable: %v", err)
	}
	fd.ExpirePartURLs()
	if _, err := c.PutPart(ctx, p3.URL, nil, bytes.NewReader(data[2*tk.PartSize:3*tk.PartSize]), tk.PartSize); !driveapi.IsStorageURLExpired(err) {
		t.Fatalf("expired url: %v", err)
	}
	st, err := c.GetUpload(ctx, tk.UploadID)
	if err != nil || len(st.Uploaded) != 2 {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, err := c.Complete(ctx, tk.UploadID, driveapi.CompleteUpload{}); err == nil {
		t.Fatal("complete with missing parts must fail")
	}
	tk2, err := c.StartUpload(ctx, req)
	if err != nil || !tk2.Resumed || len(tk2.Uploaded) != 2 || len(tk2.Parts) != 3 {
		t.Fatalf("resume %+v %v", tk2, err)
	}
	n, err := upload(ctx, c, tk2, data)
	if err != nil {
		t.Fatal(err)
	}
	if n2, err := c.Complete(ctx, tk.UploadID, driveapi.CompleteUpload{}); err != nil || n2.ID != n.ID {
		t.Fatalf("second complete %v %v", n2, err)
	}
	if n.ModifiedAt == nil || !n.ModifiedAt.Equal(mt) {
		t.Fatalf("modified_at %v", n.ModifiedAt)
	}
	d, err := c.Download(ctx, n.ID, n.VersionID(), 100000, 50000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(d.Body)
	d.Body.Close()
	if err != nil || !bytes.Equal(got, data[100000:150000]) {
		t.Fatalf("range read differs (%d bytes) %v", len(got), err)
	}
	res, err := c.CheckUploads(ctx, []driveapi.CheckFile{
		{ParentID: fd.RootID(), Name: "big.bin", Size: int64(len(data)), Fingerprint: "v1|big.bin|300k"},
		{ParentID: fd.RootID(), Name: "BIG.bin", Size: 1},
		{ParentID: fd.RootID(), Name: "other.bin", Size: 1},
	}, false)
	if err != nil || res[0].State != driveapi.CheckPresent || res[1].State != driveapi.CheckDifferent || res[2].State != driveapi.CheckAbsent {
		t.Fatalf("check %+v %v", res, err)
	}
}

func TestRefusalsConflictsAndFaults(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	managed := fd.AddFolder(fd.RootID(), "Managed")
	fd.RefuseWrites(managed, driveapi.CodeManaged)
	_, err := c.StartUpload(ctx, driveapi.StartUpload{ParentID: managed, Name: "a.txt", Size: 1})
	if !driveapi.IsPermanent(err) || !strings.Contains(driveapi.Reason(err), "Test Integration") {
		t.Fatalf("managed: %v (%s)", err, driveapi.Reason(err))
	}
	sealed := fd.AddFolder(fd.RootID(), "Sealed")
	fd.RefuseWrites(sealed, driveapi.CodeSealed)
	if _, err := c.CreateFolder(ctx, sealed, "x", "", time.Time{}); !driveapi.IsPermanent(err) {
		t.Fatalf("sealed: %v", err)
	}
	f := fd.AddFile(fd.RootID(), "doc.txt", []byte("one"))
	fi, _ := fd.Lookup("/doc.txt")
	fd.PutVersion(f, []byte("two"))
	_, err = c.StartUpload(ctx, driveapi.StartUpload{ParentID: fd.RootID(), Name: "doc.txt", Size: 5, Conflict: driveapi.ConflictNewVersion, BaseVersionID: fi.VersionID})
	if !driveapi.IsVersionConflict(err) {
		t.Fatalf("version conflict: %v", err)
	}
	if _, err := c.CreateFolder(ctx, fd.RootID(), "DOC.TXT", driveapi.ConflictFail, time.Time{}); !driveapi.IsNameConflict(err) {
		t.Fatalf("name conflict: %v", err)
	}
	q := fd.AddFolder(fd.RootID(), "Q")
	fd.SetQuota(q, 10)
	if _, err := c.StartUpload(ctx, driveapi.StartUpload{ParentID: q, Name: "a", Size: 11}); !driveapi.IsPermanent(err) {
		t.Fatalf("quota: %v", err)
	}
	fd.Unavailable(300 * time.Millisecond)
	if _, err := c.Me(ctx); !driveapi.IsRetryable(err) || driveapi.RetryAfter(err) != time.Second {
		t.Fatalf("deploy: %v", err)
	}
	time.Sleep(350 * time.Millisecond)
	fd.SetOffline(true)
	if _, err := c.Me(ctx); !driveapi.IsNetwork(err) {
		t.Fatalf("offline: %v", err)
	}
	fd.SetOffline(false)
	name := "renamed.txt"
	mt := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	n, err := c.PatchNode(ctx, f, driveapi.Patch{Name: &name, ModifiedAt: &mt})
	if err != nil || n.Name != name || !n.ModifiedAt.Equal(mt) {
		t.Fatalf("patch %+v %v", n, err)
	}
	if err := c.TrashNode(ctx, f); err != nil {
		t.Fatal(err)
	}
	if _, ok := fd.Lookup("/renamed.txt"); ok {
		t.Fatal("trashed file still listed")
	}
}

func TestStreamWakeups(t *testing.T) {
	fd, c := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opened := make(chan struct{})
	got := make(chan driveapi.StreamChange, 4)
	go func() {
		_ = c.Stream(ctx, func() { close(opened) }, func(ev driveapi.StreamEvent) error {
			var ch driveapi.StreamChange
			if ev.Event == "change" && json.Unmarshal(ev.Data, &ch) == nil {
				got <- ch
			}
			return nil
		})
	}()
	<-opened
	id := fd.AddFolder(fd.RootID(), "New")
	select {
	case ch := <-got:
		if ch.NodeID != id {
			t.Fatalf("change for %s, want %s", ch.NodeID, id)
		}
	case <-ctx.Done():
		t.Fatal("no wake-up")
	}
}

func TestRelease(t *testing.T) {
	fd, c := setup(t)
	ctx := ctxT(t)
	fd.SetRelease("stable", fakedrive.ReleaseBody("stable", []byte(`{"schema":1}`), "k1", make([]byte, 64)))
	r, err := c.Release(ctx, "stable", "")
	if err != nil || string(r.Manifest) != `{"schema":1}` || len(r.Signature) != 64 || r.KeyID != "k1" {
		t.Fatalf("release %+v %v", r, err)
	}
	if again, err := c.Release(ctx, "stable", r.ETag); err != nil || again != nil {
		t.Fatalf("not modified: %v %v", again, err)
	}
	if _, err := c.Release(ctx, "beta", ""); !driveapi.IsNotFound(err) {
		t.Fatalf("beta: %v", err)
	}
}
