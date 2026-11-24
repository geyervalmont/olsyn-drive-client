package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/core"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// harness runs the real agent over the real core against a fake drive.
type harness struct {
	t      *testing.T
	fd     *fakedrive.Server
	core   *core.FS
	client *agent.Client
	home   string
	cancel context.CancelFunc
	done   chan struct{}
}

type quietSystem struct{}

func (quietSystem) OpenURL(context.Context, string) error    { return nil }
func (quietSystem) OpenFolder(context.Context, string) error { return nil }
func (quietSystem) SetAutostart(bool) error                  { return nil }
func (quietSystem) DetectDriver() (platform.DriverInfo, error) {
	return platform.DriverInfo{Name: "none"}, nil
}
func (quietSystem) InstallDriver(context.Context, func(string)) error { return nil }
func (quietSystem) DriveLettersInUse() (map[string]string, error)     { return nil, nil }
func (quietSystem) DownloadsDir() (string, error)                     { return os.TempDir(), nil }
func (quietSystem) FreeDiskBytes(string) (uint64, error)              { return 100 << 30, nil }
func (quietSystem) ShowUI(context.Context, string) error              { return agent.ErrNoUI }

type noMount struct{}

func (noMount) Mount(context.Context, string) (string, error) { return "/mnt/olsyn", nil }
func (noMount) Unmount(context.Context, bool) error           { return nil }
func (noMount) OpenFiles() int                                { return 0 }

var fast = driveapi.Backoff{Base: 20 * time.Millisecond, Max: 200 * time.Millisecond}

func start(t *testing.T) *harness {
	t.Helper()
	fd := fakedrive.New(fakedrive.Options{PartSize: 64 << 10, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	// a short folder name: unix socket paths are limited to ~100 bytes
	home, err := os.MkdirTemp("", "odc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	paths := platform.Under(home)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	ep, err := platform.Endpoint(paths)
	if err != nil {
		t.Fatal(err)
	}
	l, err := platform.Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.New(core.Options{StateDir: paths.State, PCName: "TestPC", HTTPClient: http.DefaultClient,
		PollInterval: 200 * time.Millisecond, Backoff: fast, DisableStream: true, ReleaseInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Options{Version: "1.0.0-test", Paths: paths, Listener: l, Server: fd.URL(),
		DriveFor: func(u string) agent.Drive { return core.NewDrive(u, core.DriveOptions{HTTPClient: http.DefaultClient}) },
		FS:       c, Mounter: noMount{}, Creds: platform.NewMemoryCredentialStore(), System: quietSystem{},
		HeartbeatInterval: 200 * time.Millisecond, MinLinkInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, fd: fd, core: c, home: home, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		_, _ = a.Run(ctx)
	}()
	t.Cleanup(h.stop)
	h.client = agent.NewClient(func(ctx context.Context) (net.Conn, error) { return platform.Dial(ctx, ep) })
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		h.t.Error("agent did not stop")
	}
}

func (h *harness) waitState(what string, cond func(agent.State) bool) agent.State {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := h.client.State(ctx)
		cancel()
		if err == nil && cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s (last %+v, %v)", what, st.Connection, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (h *harness) signIn() agent.State {
	h.t.Helper()
	var si agent.SignIn
	if err := h.client.Do(context.Background(), http.MethodPost, "/v1/signin", map[string]bool{"open_browser": false}, &si); err != nil {
		h.t.Fatal(err)
	}
	if si.UserCode == "" || si.State != "waiting" {
		h.t.Fatalf("signin %+v", si)
	}
	h.fd.ApproveLinks()
	return h.waitState("connected", func(s agent.State) bool { return s.Connection == agent.ConnConnected && s.Ready })
}

func (h *harness) save(path string, data []byte) {
	h.t.Helper()
	v := h.core.VFS()
	if v == nil {
		h.t.Fatal("no file system")
	}
	ctx := context.Background()
	f, err := v.Open(ctx, path, vfs.OpenFlags{Create: true, Truncate: true, Access: vfs.AccessRead | vfs.AccessWrite, Share: vfs.ShareRead})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := f.WriteAt(ctx, data, 0); err != nil {
		h.t.Fatal(err)
	}
	if err := f.Cleanup(ctx, false); err != nil {
		h.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		h.t.Fatal(err)
	}
}

func TestSignInSaveListSignOut(t *testing.T) {
	h := start(t)
	st := h.signIn()
	if st.Account == nil || st.Account.Name != "Test Person" || st.Account.Server != h.fd.URL() {
		t.Fatalf("account %+v", st.Account)
	}
	data := bytes.Repeat([]byte("olsyn "), 50000) // 300 kB: a multipart upload
	h.save("/report.txt", data)
	deadline := time.Now().Add(15 * time.Second)
	var page agent.TransferPage
	for {
		if err := h.client.Do(context.Background(), http.MethodGet, "/v1/transfers?state=done", nil, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no finished transfer: %+v", page)
		}
		time.Sleep(50 * time.Millisecond)
	}
	tr := page.Items[0]
	if tr.Name != "report.txt" || tr.DrivePath != "/report.txt" || tr.Size != int64(len(data)) || tr.Origin != "mount" || tr.FinishedAt == nil {
		t.Fatalf("transfer %+v", tr)
	}
	if fi, ok := h.fd.Lookup("/report.txt"); !ok || !bytes.Equal(fi.Data, data) {
		t.Fatal("file not on the drive")
	}
	if hb := h.fd.LastHeartbeat(); hb == nil || hb.AppVersion != "1.0.0-test" {
		t.Fatalf("heartbeat %+v", hb)
	}

	// sign out: the device is unlinked on the drive, so its token stops working
	var out agent.SignOutResult
	if err := h.client.Do(context.Background(), http.MethodPost, "/v1/signout", map[string]bool{"clear_cache": true}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.SignedOut || !out.Unlinked || !out.CacheCleared {
		t.Fatalf("signout %+v", out)
	}
	h.waitState("signed out", func(s agent.State) bool { return s.Connection == agent.ConnSignedOut })
	accounts, _ := os.ReadDir(filepath.Join(h.home, "state", "accounts"))
	if len(accounts) != 0 {
		t.Fatalf("cache not cleared: %d account folders", len(accounts))
	}
}

func TestDriveFoldersAndCache(t *testing.T) {
	h := start(t)
	proj := h.fd.AddFolder(h.fd.RootID(), "Projects")
	h.fd.AddFolder(proj, "Tower")
	sealed := h.fd.AddFolder(proj, "Issued")
	h.signIn()
	h.fd.RefuseWrites(sealed, driveapi.CodeSealed)
	ctx := context.Background()
	var df agent.DriveFolders
	if err := h.client.Do(ctx, http.MethodGet, "/v1/drive/folders?parent=/Projects", nil, &df); err != nil {
		t.Fatal(err)
	}
	if df.ParentID != proj || len(df.Folders) != 2 {
		t.Fatalf("folders %+v", df)
	}
	h.save("/a.bin", bytes.Repeat([]byte{1}, 1000))
	var cu agent.CacheUsage
	if err := h.client.Do(ctx, http.MethodGet, "/v1/cache", nil, &cu); err != nil {
		t.Fatal(err)
	}
	if cu.UsedBytes < 1000 || cu.CapBytes == 0 {
		t.Fatalf("cache %+v", cu)
	}
}

func waitBatch(t *testing.T, h *harness, id string, cond func(agent.Batch) bool) agent.Batch {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var res struct{ Items []agent.Batch }
		if err := h.client.Do(context.Background(), http.MethodGet, "/v1/batches", nil, &res); err != nil {
			t.Fatal(err)
		}
		for _, b := range res.Items {
			if b.ID == id && cond(b) {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch %s: %+v", id, res.Items)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestSendThroughTheAgent(t *testing.T) {
	h := start(t)
	proj := h.fd.AddFolder(h.fd.RootID(), "Projects")
	sealed := h.fd.AddFolder(proj, "Issued")
	h.signIn()
	h.fd.RefuseWrites(sealed, driveapi.CodeSealed)
	src := filepath.Join(t.TempDir(), "Exports")
	os.MkdirAll(filepath.Join(src, "Sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(src, "Sub", "b.bin"), bytes.Repeat([]byte{7}, 200<<10), 0o644)
	single := filepath.Join(t.TempDir(), "plan.pdf")
	os.WriteFile(single, []byte("plan"), 0o644)
	ctx := context.Background()

	// paused: the batch is found but nothing moves until resumed
	if err := h.client.Do(ctx, http.MethodPost, "/v1/pause", nil, nil); err != nil {
		t.Fatal(err)
	}
	var res agent.SendResult
	body := map[string]any{"local_paths": []string{src, single}, "drive_folder": "/Projects"}
	if err := h.client.Do(ctx, http.MethodPost, "/v1/send", body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes != 5+200<<10+4 {
		t.Fatalf("send result %+v", res)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := h.fd.Lookup("/Projects/Exports/Sub/b.bin"); ok {
		t.Fatal("sent while paused")
	}
	var page agent.TransferPage
	if err := h.client.Do(ctx, http.MethodGet, "/v1/transfers?state=active,waiting", nil, &page); err != nil {
		t.Fatal(err)
	}
	if page.Counts.Active+page.Counts.Waiting != 3 || page.Items[0].Origin != "send" || page.Items[0].BatchID == nil {
		t.Fatalf("queued %+v", page)
	}
	if err := h.client.Do(ctx, http.MethodPost, "/v1/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	b := waitBatch(t, h, res.BatchID, func(b agent.Batch) bool { return b.State == agent.BatchDone })
	if b.FilesDone != 3 || b.BytesDone != b.Bytes || b.Files != 3 {
		t.Fatalf("batch %+v", b)
	}
	if fi, ok := h.fd.Lookup("/Projects/Exports/Sub/b.bin"); !ok || len(fi.Data) != 200<<10 {
		t.Fatal("folder not on the drive")
	}
	if _, ok := h.fd.Lookup("/Projects/plan.pdf"); !ok {
		t.Fatal("file not on the drive")
	}
	if _, err := os.Stat(filepath.Join(src, "Sub", "b.bin")); err != nil {
		t.Fatal("the source was touched")
	}

	// into a sealed folder: refused up front
	var ae *agent.APIError
	err := h.client.Do(ctx, http.MethodPost, "/v1/send", map[string]any{"local_paths": []string{single}, "drive_folder": "/Projects/Issued"}, nil)
	if !errors.As(err, &ae) || ae.Code != "drive_folder_read_only" {
		t.Fatalf("sealed destination: %v", err)
	}
	// a name taken by different content: held, listed, discardable
	os.WriteFile(single, []byte("plan, changed"), 0o644)
	if err := h.client.Do(ctx, http.MethodPost, "/v1/send", map[string]any{"local_path": single, "drive_folder": "/Projects"}, &res); err != nil {
		t.Fatal(err)
	}
	waitBatch(t, h, res.BatchID, func(b agent.Batch) bool { return b.State == agent.BatchDone && b.FilesHeld == 1 })
	if err := h.client.Do(ctx, http.MethodGet, "/v1/transfers?state=held", nil, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Held.Code != driveapi.CodeNameConflict {
		t.Fatalf("held %+v", page.Items)
	}
	if err := h.client.Do(ctx, http.MethodPost, "/v1/held/"+page.Items[0].ID+"/discard", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.waitState("nothing held", func(s agent.State) bool { return s.Transfers.Held == 0 })
}

func TestUnfinishedSendCarriesOnAfterRestart(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{PartSize: 64 << 10, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	state, cache := t.TempDir(), t.TempDir()
	sess := agent.Session{ServerURL: fd.URL(), Token: fd.Token(), UserID: "u1", DeviceID: "d1", CacheDir: cache, CacheCapBytes: 1 << 30}
	src := filepath.Join(t.TempDir(), "Many")
	os.MkdirAll(src, 0o755)
	for i := range 30 {
		os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.bin", i)), bytes.Repeat([]byte{byte(i)}, 150<<10), 0o644)
	}
	open := func() *core.FS {
		c, err := core.New(core.Options{StateDir: state, HTTPClient: http.DefaultClient, PollInterval: time.Second, Backoff: fast, DisableStream: true})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range c.Events() {
			}
		}()
		if err := c.Start(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	fd.SetLatency(20 * time.Millisecond)
	res, err := c.Send(context.Background(), agent.SendRequest{LocalPaths: []string{src}, DriveFolder: "/"})
	if err != nil {
		t.Fatal(err)
	}
	for len(fd.List(fd.RootID())) == 0 || countDone(c, res.BatchID) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fd.SetLatency(0)
	c2 := open()
	defer c2.Stop(context.Background())
	deadline := time.Now().Add(20 * time.Second)
	for {
		bs := c2.Batches()
		if len(bs) == 1 && bs[0].State == agent.BatchDone {
			if bs[0].FilesDone+bs[0].FilesSkipped != 30 {
				t.Fatalf("after restart %+v", bs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not finished: %+v", bs)
		}
		time.Sleep(30 * time.Millisecond)
	}
	for i := range 30 {
		if fi, ok := fd.Lookup(fmt.Sprintf("/Many/f%02d.bin", i)); !ok || len(fi.Data) != 150<<10 {
			t.Fatalf("f%02d missing", i)
		}
	}
}

func countDone(c *core.FS, id string) int {
	for _, b := range c.Batches() {
		if b.ID == id {
			return b.FilesDone
		}
	}
	return 0
}

func TestRevokedTokenSignsOut(t *testing.T) {
	h := start(t)
	h.signIn()
	h.fd.RevokeDevices()
	st := h.waitState("signed out by the drive", func(s agent.State) bool { return s.Connection == agent.ConnSignedOut })
	if st.Reason == nil || st.Reason.Code != "token_revoked" {
		t.Fatalf("reason %+v", st.Reason)
	}
}

func TestHeldFileIsListedWithReason(t *testing.T) {
	h := start(t)
	sealed := h.fd.AddFolder(h.fd.RootID(), "Sealed")
	h.signIn()
	// the folder is sealed after this PC listed it: the refusal comes at upload
	if _, _, err := h.core.VFS().ReadDir(context.Background(), "/Sealed", "", 0); err != nil {
		t.Fatal(err)
	}
	h.fd.RefuseWrites(sealed, driveapi.CodeSealed)
	h.save("/Sealed/new.txt", []byte("hello"))
	deadline := time.Now().Add(15 * time.Second)
	var page agent.TransferPage
	for {
		if err := h.client.Do(context.Background(), http.MethodGet, "/v1/transfers?state=held", nil, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing held")
		}
		time.Sleep(50 * time.Millisecond)
	}
	it := page.Items[0]
	if it.Held == nil || it.Held.Code != driveapi.CodeSealed || len(it.Held.Actions) != 2 {
		t.Fatalf("held %+v", it.Held)
	}
	st := h.waitState("attention", func(s agent.State) bool { return s.Connection == agent.ConnAttention })
	if st.Transfers.Held != 1 {
		t.Fatalf("summary %+v", st.Transfers)
	}
	// the sign-out warning counts it
	var e agent.APIError
	err := h.client.Do(context.Background(), http.MethodPost, "/v1/signout", map[string]bool{"clear_cache": true}, nil)
	if ae, ok := err.(*agent.APIError); !ok || ae.Code != "unsent_data" {
		t.Fatalf("signout with a held file: %v %v", err, e)
	}
	// save a copy elsewhere: it leaves the held list
	dest := t.TempDir()
	var saved struct{ Path string }
	if err := h.client.Do(context.Background(), http.MethodPost, "/v1/held/"+it.ID+"/save-copy", map[string]string{"dest_dir": dest}, &saved); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(saved.Path); err != nil || string(b) != "hello" {
		t.Fatalf("saved copy: %v", err)
	}
	h.waitState("held list empty", func(s agent.State) bool { return s.Transfers.Held == 0 })
}
