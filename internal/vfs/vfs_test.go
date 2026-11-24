package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

const part = 64 << 10

type env struct {
	t        *testing.T
	fd       *fakedrive.Server
	api      *driveapi.Client
	dir      string
	cacheCap int64
	attempts int
	// release timings and how often they run (zero: the defaults, which
	// release nothing within a test)
	release      cache.ReleasePolicy
	releaseEvery time.Duration
	keepOffline  func(string) bool
	// stream turns the SSE wake-ups on; poll is the feed's idle poll
	// (zero: 200 ms)
	stream bool
	poll   time.Duration
	// realFeed: the drive limits the feed and the client paces itself
	// with its production settings
	realFeed bool

	fs    *vfs.FS
	idx   *index.DB
	cache *cache.Cache
	jr    *journal.Journal
	eng   *transfer.Simple
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10})
}

func newEnvWith(t *testing.T, opts fakedrive.Options) *env {
	t.Helper()
	// The feed's limit (60 calls a minute) and the client's pace under it
	// are modelled only when a test asks for the limit: elsewhere the feed
	// is read every 200 ms, quickly after every wake-up.
	realFeed := opts.ChangesRate > 0
	limits := driveapi.DefaultLimits()
	if !realFeed {
		opts.ChangesRate = -1
		delete(limits, driveapi.ClassChanges)
	}
	fd := fakedrive.New(opts)
	t.Cleanup(fd.Close)
	api, err := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return fd.Token(), nil },
		Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, fd: fd, api: api, dir: t.TempDir(), cacheCap: 64 << 20, attempts: 3, realFeed: realFeed}
}

var fast = driveapi.Backoff{Base: 20 * time.Millisecond, Max: 200 * time.Millisecond}

func (e *env) start() *env {
	t := e.t
	t.Helper()
	var err error
	if e.idx, err = index.Open(filepath.Join(e.dir, "index.db")); err != nil {
		t.Fatal(err)
	}
	if e.cache, err = cache.Open(cache.Options{Dir: filepath.Join(e.dir, "cache"), Cap: e.cacheCap, BlockSize: 16 << 10,
		BackpressureTimeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if e.jr, err = journal.Open(filepath.Join(e.dir, "journal.log")); err != nil {
		t.Fatal(err)
	}
	var fsRef atomic.Pointer[vfs.FS]
	e.eng = transfer.NewSimple(e.api, transfer.Options{Parallel: 4, Attempts: e.attempts, Backoff: fast,
		OnEvent: func(ev transfer.Event) {
			if f := fsRef.Load(); f != nil {
				f.TransferEvent(ev)
			}
		}})
	poll := e.poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	minI, maxI, perMinute := 20*time.Millisecond, 100*time.Millisecond, -1
	if e.realFeed {
		minI, maxI, perMinute = 0, 0, 0 // the defaults
	}
	e.fs, err = vfs.New(vfs.Config{API: e.api, Engine: e.eng, Index: e.idx, Cache: e.cache, Journal: e.jr,
		RootID: e.fd.RootID(), PCName: "TestPC", PollInterval: poll, Backoff: fast,
		ReadTimeout: 5 * time.Second, DisableStream: !e.stream, ReadAhead: 64 << 10,
		ChangesMinInterval: minI, ChangesMaxInterval: maxI, ChangesPerMinute: perMinute,
		Release: e.release, ReleaseInterval: e.releaseEvery, KeepOffline: e.keepOffline})
	if err != nil {
		t.Fatal(err)
	}
	fsRef.Store(e.fs)
	e.fs.Start(context.Background())
	return e
}

// kill drops everything as a crashed process would: nothing flushed.
func (e *env) kill() {
	e.fs.Stop()
	e.cache.Abandon()
	e.jr.Abandon()
	_ = e.idx.Close()
}

func (e *env) stop() {
	if err := e.fs.Close(); err != nil {
		e.t.Error(err)
	}
	_ = e.cache.Close()
	_ = e.jr.Close()
	_ = e.idx.Close()
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+i/7) ^ seed
	}
	return b
}

var rw = vfs.OpenFlags{Access: vfs.AccessRead | vfs.AccessWrite, Share: vfs.ShareRead}

// writeFile copies data in like Explorer: create/overwrite, write in 64 KiB
// chunks, set the modified time, close.
func (e *env) writeFile(path string, data []byte, mtime time.Time) error {
	ctx := ctxT(e.t)
	fl := rw
	fl.Create, fl.Truncate = true, true
	h, err := e.fs.Open(ctx, path, fl)
	if err != nil {
		return err
	}
	defer h.Close()
	for off := 0; off < len(data); off += part {
		end := min(off+part, len(data))
		if _, err := h.WriteAt(ctx, data[off:end], int64(off)); err != nil {
			return err
		}
	}
	if !mtime.IsZero() {
		if err := h.SetTimes(ctx, mtime, time.Time{}); err != nil {
			return err
		}
	}
	return h.Cleanup(ctx, false)
}

func (e *env) readFile(path string) ([]byte, error) {
	ctx := ctxT(e.t)
	h, err := e.fs.Open(ctx, path, vfs.OpenFlags{Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		return nil, err
	}
	defer h.Close()
	var out []byte
	buf := make([]byte, 50000)
	for off := int64(0); ; {
		n, err := h.ReadAt(ctx, buf, off)
		out = append(out, buf[:n]...)
		off += int64(n)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func (e *env) mkdir(path string) {
	e.t.Helper()
	h, err := e.fs.Open(ctxT(e.t), path, vfs.OpenFlags{Create: true, Exclusive: true, Directory: true, Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		e.t.Fatal(err)
	}
	_ = h.Close()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) waitSynced(d time.Duration) {
	e.t.Helper()
	// the file system's count, not the journal's: the journal empties a
	// moment before the file system has recorded each change as sent
	waitFor(e.t, d, "the queue to drain", func() bool { return e.fs.Pending() == 0 })
}

func TestCopyManySmallFilesAndOneLarge(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	e.mkdir("/Batch")
	mt := time.Date(2025, 6, 7, 8, 9, 10, 123e6, time.UTC)
	t0 := time.Now()
	for i := 0; i < 300; i++ {
		if err := e.writeFile(fmt.Sprintf("/Batch/file-%03d.txt", i), payload(1000+i*7, byte(i)), mt); err != nil {
			t.Fatal(err)
		}
	}
	big := payload(23*part+999, 9)
	if err := e.writeFile("/Batch/big.bin", big, mt); err != nil {
		t.Fatal(err)
	}
	t.Logf("301 files written locally in %v", time.Since(t0))
	// written data is readable at once, before it reaches the drive
	if got, err := e.readFile("/Batch/big.bin"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("local read before upload: %v", err)
	}
	e.waitSynced(60 * time.Second)
	for i := 0; i < 300; i += 37 {
		fi, ok := e.fd.Lookup(fmt.Sprintf("/Batch/file-%03d.txt", i))
		if !ok || !bytes.Equal(fi.Data, payload(1000+i*7, byte(i))) {
			t.Fatalf("file %d missing or wrong on the drive", i)
		}
		if fi.ModifiedAt == nil || !fi.ModifiedAt.Equal(mt) {
			t.Fatalf("file %d modified_at %v", i, fi.ModifiedAt)
		}
	}
	if fi, ok := e.fd.Lookup("/Batch/big.bin"); !ok || !bytes.Equal(fi.Data, big) || fi.Versions != 1 {
		t.Fatal("big.bin wrong on the drive")
	}
	if n := len(e.fd.List(e.mustSID("/Batch"))); n != 301 {
		t.Fatalf("drive has %d files", n)
	}
	infos, _, err := e.fs.ReadDir(ctxT(t), "/Batch", "", 0)
	if err != nil || len(infos) != 301 {
		t.Fatalf("listing %d %v", len(infos), err)
	}
	st, _ := e.fs.Stat(ctxT(t), "/Batch/big.bin")
	if st.Offline || st.Pending || st.Size != int64(len(big)) || !st.MTime.Equal(mt) {
		t.Fatalf("stat after upload %+v", st)
	}
}

// saveOnly does what Explorer's copy engine does for one file up to its
// CloseHandle (create, write, set the time, cleanup) and returns the handle
// for the Close that WinFsp delivers later from its own threads.
func (e *env) saveOnly(path string, data []byte, mtime time.Time) (*vfs.Handle, error) {
	ctx := ctxT(e.t)
	fl := rw
	fl.Create, fl.Truncate = true, true
	h, err := e.fs.Open(ctx, path, fl)
	if err != nil {
		return nil, err
	}
	if _, err := h.WriteAt(ctx, data, 0); err != nil {
		_ = h.Close()
		return nil, err
	}
	if err := h.SetTimes(ctx, mtime, time.Time{}); err != nil {
		_ = h.Close()
		return nil, err
	}
	if err := h.Cleanup(ctx, false); err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}

// Many small saves: the app's per-file path writes the journal without an
// fsync, Closes (from WinFsp's threads) share fsyncs, a Close returns only
// once its file's change is on the device, and after a crash every saved
// file is still queued and reaches the drive.
//
// Before this design every Close appended to a bbolt journal with two
// fsyncs of its own: TestCopyManySmallFilesAndOneLarge took 15.3 s for 301
// files on an idle M-series Mac (51 ms a file) and about 40 s under load.
func TestManySmallSavesAreFastAndDurable(t *testing.T) {
	e := newEnv(t).start()
	e.mkdir("/Small")
	e.waitSynced(10 * time.Second)
	e.fd.SetOffline(true) // nothing leaves: everything must come back from the journal
	mt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	// one fsync per file: each Close waits before the next file starts
	const inline = 100
	syncs0 := e.jr.Stats().Syncs
	t0 := time.Now()
	for i := range inline {
		h, err := e.saveOnly(fmt.Sprintf("/Small/inline-%04d.txt", i), payload(200+i, byte(i)), mt)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	perInline := time.Since(t0) / inline
	inlineSyncs := e.jr.Stats().Syncs - syncs0

	// WinFsp-style: Close arrives asynchronously on 8 threads
	const n = 2000
	type closeReq struct {
		h       *vfs.Handle
		written uint64
	}
	closes := make(chan closeReq, 64)
	var wg sync.WaitGroup
	var early atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range closes {
				if err := c.h.Close(); err != nil {
					t.Error(err)
				}
				if e.jr.Stats().Synced < c.written {
					early.Add(1)
				}
			}
		}()
	}
	syncs0 = e.jr.Stats().Syncs
	t1 := time.Now()
	for i := range n {
		h, err := e.saveOnly(fmt.Sprintf("/Small/file-%05d.txt", i), payload(200+i%500, byte(i)), mt)
		if err != nil {
			t.Fatal(err)
		}
		closes <- closeReq{h, e.jr.Stats().Written}
	}
	appDone := time.Since(t1)
	close(closes)
	wg.Wait()
	all := time.Since(t1)
	asyncSyncs := e.jr.Stats().Syncs - syncs0
	t.Logf("before (Close waits per file): %d files, %v a file, %d fsyncs", inline, perInline, inlineSyncs)
	t.Logf("after (Closes share fsyncs):  %d files, %v a file for the app, %v a file until every Close returned, %d fsyncs",
		n, appDone/n, all/n, asyncSyncs)
	if early.Load() > 0 {
		t.Fatalf("%d Closes returned before their change was durable", early.Load())
	}
	if asyncSyncs >= n/2 {
		t.Fatalf("no group commit: %d fsyncs for %d files", asyncSyncs, n)
	}

	// crash, come back online: every saved file is still queued and is sent
	e.kill()
	e.fd.SetOffline(false)
	e.start()
	defer e.stop()
	if got := e.fs.Pending(); got != inline+n {
		t.Fatalf("after the crash %d changes queued, want %d", got, inline+n)
	}
	e.waitSynced(120 * time.Second)
	if got := len(e.fd.List(e.mustSID("/Small"))); got != inline+n {
		t.Fatalf("drive has %d files, want %d", got, inline+n)
	}
	for _, i := range []int{0, 999, n - 1} {
		fi, ok := e.fd.Lookup(fmt.Sprintf("/Small/file-%05d.txt", i))
		if !ok || !bytes.Equal(fi.Data, payload(200+i%500, byte(i))) || !fi.ModifiedAt.Equal(mt) {
			t.Fatalf("file %d wrong on the drive", i)
		}
	}
}

// A power loss can leave a saved file's data unwritten while its journal
// record survived. Recovery checks the record's CRC and holds the file
// rather than sending damaged content.
func TestPowerLossDamageIsHeldNotSent(t *testing.T) {
	e := newEnv(t).start()
	if _, _, err := e.fs.ReadDir(ctxT(t), "/", "", 0); err != nil {
		t.Fatal(err)
	}
	e.fd.SetOffline(true)
	data := payload(5000, 3)
	if err := e.writeFile("/report.txt", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.kill()
	// simulate the lost data: the file is there, its bytes are not
	files, _ := os.ReadDir(filepath.Join(e.dir, "cache", "data"))
	if len(files) != 1 {
		t.Fatalf("%d data files", len(files))
	}
	if err := os.WriteFile(filepath.Join(e.dir, "cache", "data", files[0].Name()), make([]byte, len(data)), 0o600); err != nil {
		t.Fatal(err)
	}
	// and the cache's own metadata was not yet written either
	_ = os.Remove(filepath.Join(e.dir, "cache", "cache.db"))
	e.fd.SetOffline(false)
	e.start()
	defer e.stop()
	waitFor(t, 5*time.Second, "the damaged file to be held", func() bool { return len(e.fs.Held()) == 1 })
	if h := e.fs.Held()[0]; !strings.Contains(h.Reason, "damaged") {
		t.Fatalf("held for %q", h.Reason)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := e.fd.Lookup("/report.txt"); ok {
		t.Fatal("damaged content was sent")
	}
}

func (e *env) mustSID(path string) string {
	e.t.Helper()
	fi, ok := e.fd.Lookup(path)
	if !ok {
		e.t.Fatalf("%s not on the drive", path)
	}
	return fi.ID
}

func TestDeployMidUploadResumesFromLastPart(t *testing.T) {
	e := newEnv(t)
	e.attempts = 2 // give up quickly so the journal's resume path is used
	e.start()
	defer e.stop()
	var puts, injected atomic.Int32
	var deploy sync.Once
	e.fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		if puts.Add(1) > 10 {
			deploy.Do(func() { e.fd.Unavailable(1500 * time.Millisecond) })
		}
		if puts.Load() > 10 && puts.Load() < 20 { // storage behind the same ingress is down too
			injected.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	data := payload(30*part, 3)
	if err := e.writeFile("/deploy.bin", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(30 * time.Second)
	fi, ok := e.fd.Lookup("/deploy.bin")
	if !ok || !bytes.Equal(fi.Data, data) {
		t.Fatal("content differs after the deploy")
	}
	if sent := int(puts.Load() - injected.Load()); sent != 30 {
		t.Fatalf("%d parts stored for 30: finished parts were sent again", sent)
	}
	if e.fd.PendingUploads() != 0 {
		t.Fatal("an upload session was left behind")
	}
}

func TestKillAndReopenResumesUpload(t *testing.T) {
	e := newEnv(t).start()
	var puts, injected atomic.Int32
	var block atomic.Bool
	block.Store(true)
	e.fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		n := puts.Add(1)
		if block.Load() && n > 8 {
			injected.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	data := payload(20*part+5, 4)
	mt := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := e.writeFile("/kill.bin", data, mt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "8 parts stored", func() bool { return puts.Load()-injected.Load() >= 8 && injected.Load() > 0 })
	e.kill()
	if e.fd.PendingUploads() != 1 {
		t.Fatalf("pending uploads on the drive: %d", e.fd.PendingUploads())
	}
	block.Store(false)
	e.start()
	defer e.stop()
	e.waitSynced(30 * time.Second)
	fi, ok := e.fd.Lookup("/kill.bin")
	if !ok || !bytes.Equal(fi.Data, data) || !fi.ModifiedAt.Equal(mt) {
		t.Fatal("content or time differs after reopen")
	}
	if sent := int(puts.Load() - injected.Load()); sent != 21 {
		t.Fatalf("%d parts stored for 21: the resume re-sent finished parts", sent)
	}
}

func TestNetworkDropFailsFastAndRecovers(t *testing.T) {
	e := newEnv(t)
	e.fd.AddFile(e.fd.RootID(), "remote.txt", payload(5000, 1))
	sub := e.fd.AddFolder(e.fd.RootID(), "Later")
	e.fd.AddFile(sub, "x.txt", []byte("x"))
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	if _, _, err := e.fs.ReadDir(ctx, "/", "", 0); err != nil {
		t.Fatal(err)
	}
	e.fd.SetOffline(true)
	if err := e.writeFile("/made-offline.txt", []byte("written while offline"), time.Time{}); err != nil {
		t.Fatalf("writes must work offline: %v", err)
	}
	waitFor(t, 10*time.Second, "offline state", func() bool { return e.fs.State() == vfs.StateOffline })
	t0 := time.Now()
	if _, err := e.readFile("/remote.txt"); vfs.KindOf(err) != vfs.KindOffline {
		t.Fatalf("uncached read offline: %v", err)
	}
	if _, _, err := e.fs.ReadDir(ctx, "/Later", "", 0); vfs.KindOf(err) != vfs.KindOffline {
		t.Fatalf("unlisted folder offline: %v", err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("offline operations took %v", el)
	}
	if infos, _, err := e.fs.ReadDir(ctx, "/", "", 0); err != nil || len(infos) != 3 {
		t.Fatalf("listing from the index offline: %d %v", len(infos), err)
	}
	if got, err := e.readFile("/made-offline.txt"); err != nil || string(got) != "written while offline" {
		t.Fatalf("local file offline: %q %v", got, err)
	}
	e.fd.SetOffline(false)
	e.waitSynced(20 * time.Second)
	waitFor(t, 5*time.Second, "online state", func() bool { return e.fs.State() == vfs.StateOnline })
	if fi, ok := e.fd.Lookup("/made-offline.txt"); !ok || string(fi.Data) != "written while offline" {
		t.Fatal("offline write not uploaded")
	}
	if got, err := e.readFile("/remote.txt"); err != nil || !bytes.Equal(got, payload(5000, 1)) {
		t.Fatalf("read after recovery: %v", err)
	}
}

func TestManagedFolderRefusalIsHeldNotRetried(t *testing.T) {
	e := newEnv(t)
	managed := e.fd.AddFolder(e.fd.RootID(), "Managed")
	e.fd.RefuseWrites(managed, driveapi.CodeManaged)
	sealed := e.fd.AddFolder(e.fd.RootID(), "Sealed")
	e.fd.RefuseWrites(sealed, driveapi.CodeSealed)
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	data := payload(3000, 5)
	if err := e.writeFile("/Managed/report.pdf", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "a held change", func() bool { return len(e.fs.Held()) == 1 })
	held := e.fs.Held()[0]
	if held.Code != driveapi.CodeManaged || !strings.Contains(held.Reason, "managed") || held.Path != "/Managed/report.pdf" {
		t.Fatalf("held %+v", held)
	}
	starts := e.fd.Count("upload.start")
	time.Sleep(600 * time.Millisecond)
	if e.fd.Count("upload.start") != starts {
		t.Fatal("a refused upload was retried")
	}
	// local data survives and stays readable
	if got, err := e.readFile("/Managed/report.pdf"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("local copy after refusal: %v", err)
	}
	out := t.TempDir()
	p, err := e.fs.SaveCopy(ctx, held.Seq, out)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, data) {
		t.Fatal("saved copy differs")
	}
	if err := e.fs.Discard(ctx, held.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Stat(ctx, "/Managed/report.pdf"); vfs.KindOf(err) != vfs.KindNotFound {
		t.Fatalf("discarded file still shown: %v", err)
	}
	// a sealed folder refuses at once, locally
	if err := e.writeFile("/Sealed/x.txt", []byte("x"), time.Time{}); vfs.KindOf(err) != vfs.KindAccessDenied {
		t.Fatalf("sealed create: %v", err)
	}
}

func TestConflictCopy(t *testing.T) {
	e := newEnv(t)
	id := e.fd.AddFile(e.fd.RootID(), "doc.txt", []byte("version one, from the drive"))
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	h, err := e.fs.Open(ctx, "/doc.txt", rw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt(ctx, []byte("VERSION"), 0); err != nil { // an in-place edit: the rest comes from the drive
		t.Fatal(err)
	}
	e.fd.PutVersion(id, []byte("someone else saved this"))
	if err := h.Cleanup(ctx, false); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	e.waitSynced(20 * time.Second)
	names := e.fd.List(e.fd.RootID())
	if len(names) != 2 {
		t.Fatalf("drive has %v", names)
	}
	var copyName string
	for _, n := range names {
		if strings.Contains(n, "(conflict from TestPC, ") {
			copyName = n
		}
	}
	if copyName == "" {
		t.Fatalf("no conflict copy in %v", names)
	}
	cp, _ := e.fd.Lookup("/" + copyName)
	if string(cp.Data) != "VERSION one, from the drive" {
		t.Fatalf("conflict copy holds %q", cp.Data)
	}
	if orig, _ := e.fd.Lookup("/doc.txt"); string(orig.Data) != "someone else saved this" {
		t.Fatal("the drive's newer version was overwritten")
	}
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "both files listed", func() bool {
		infos, _, _ := e.fs.ReadDir(ctx, "/", "", 0)
		return len(infos) == 2
	})
	if got, err := e.readFile("/doc.txt"); err != nil || string(got) != "someone else saved this" {
		t.Fatalf("local doc.txt %q %v", got, err)
	}
}

// slowUploads makes every storage PUT wait until release is closed.
func slowUploads(fd *fakedrive.Server) (release func()) {
	ch := make(chan struct{})
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			select {
			case <-ch:
			case <-r.Context().Done():
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
		}
		return false
	})
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func TestRenameBeforeUploadFinished(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	ctx := ctxT(t)
	e.mkdir("/Dest")
	e.waitSynced(10 * time.Second)
	release := slowUploads(e.fd)
	defer release()
	data := payload(5*part, 6)
	if err := e.writeFile("/draft.bin", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the upload to start", func() bool { return e.fd.Count("upload.start") > 0 })
	if err := e.fs.Rename(ctx, "/draft.bin", "/Dest/final.bin", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Stat(ctx, "/Dest/final.bin"); err != nil {
		t.Fatalf("renamed file not visible at once: %v", err)
	}
	release()
	e.waitSynced(20 * time.Second)
	if fi, ok := e.fd.Lookup("/Dest/final.bin"); !ok || !bytes.Equal(fi.Data, data) {
		t.Fatal("file not at its new name on the drive")
	}
	if _, ok := e.fd.Lookup("/draft.bin"); ok {
		t.Fatal("old name still on the drive")
	}
}

func TestDeleteBeforeUploadFinished(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	ctx := ctxT(t)
	release := slowUploads(e.fd)
	defer release()
	if err := e.writeFile("/temp.bin", payload(5*part, 7), time.Time{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the upload to start", func() bool { return e.fd.PendingUploads() > 0 })
	if err := e.fs.Remove(ctx, "/temp.bin"); err != nil {
		t.Fatal(err)
	}
	release()
	e.waitSynced(20 * time.Second)
	if fi, ok := e.fd.Lookup("/temp.bin"); ok && !fi.Trashed {
		t.Fatal("deleted file reached the drive")
	}
	waitFor(t, 5*time.Second, "the upload session to be aborted", func() bool { return e.fd.PendingUploads() == 0 })
	if _, err := e.fs.Stat(ctx, "/temp.bin"); vfs.KindOf(err) != vfs.KindNotFound {
		t.Fatalf("stat after delete: %v", err)
	}
	if st := e.cache.Stats(); st.Unsent != 0 {
		t.Fatalf("unsent bytes left in the cache: %d", st.Unsent)
	}
}

func TestCacheEvictionUnderCap(t *testing.T) {
	e := newEnv(t)
	e.cacheCap = 512 << 10
	for i := 0; i < 5; i++ {
		e.fd.AddFile(e.fd.RootID(), fmt.Sprintf("f%d.bin", i), payload(200<<10, byte(i)))
	}
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	for i := 0; i < 5; i++ {
		got, err := e.readFile(fmt.Sprintf("/f%d.bin", i))
		if err != nil || !bytes.Equal(got, payload(200<<10, byte(i))) {
			t.Fatalf("read f%d: %v", i, err)
		}
		st, _ := e.fs.Stat(ctx, fmt.Sprintf("/f%d.bin", i))
		if st.Offline {
			t.Fatalf("f%d offline right after a full read", i)
		}
	}
	e.cache.Evict()
	if st := e.cache.Stats(); st.Used > 512<<10 {
		t.Fatalf("cache holds %d bytes over its cap", st.Used)
	}
	first, _ := e.fs.Stat(ctx, "/f0.bin")
	last, _ := e.fs.Stat(ctx, "/f4.bin")
	if !first.Offline || last.Offline {
		t.Fatalf("least recently used should go first: f0 offline=%v f4 offline=%v", first.Offline, last.Offline)
	}
	before := e.fd.Count("download")
	if got, err := e.readFile("/f0.bin"); err != nil || !bytes.Equal(got, payload(200<<10, 0)) {
		t.Fatal("re-read after eviction")
	}
	if e.fd.Count("download") == before {
		t.Fatal("evicted file was not fetched again")
	}
}

func TestChangesFeedNamesJunkAndShareModes(t *testing.T) {
	e := newEnv(t)
	odd := e.fd.AddFile(e.fd.RootID(), "a:b?.txt", []byte("odd"))
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	if got, err := e.readFile("/a：b？.txt"); err != nil || string(got) != "odd" {
		t.Fatalf("mapped name: %q %v", got, err)
	}
	// drive-side changes arrive through the feed
	e.fd.AddFile(e.fd.RootID(), "new.txt", []byte("new"))
	e.fd.Rename(odd, "renamed.txt")
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "changes applied", func() bool {
		_, err1 := e.fs.Stat(ctx, "/new.txt")
		_, err2 := e.fs.Stat(ctx, "/renamed.txt")
		return err1 == nil && err2 == nil
	})
	if _, err := e.fs.Stat(ctx, "/a：b？.txt"); vfs.KindOf(err) != vfs.KindNotFound {
		t.Fatalf("old name still listed: %v", err)
	}
	// junk stays local
	if err := e.writeFile("/Thumbs.db", []byte("junk"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.writeFile("/~$report.docx", []byte("owner"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(5 * time.Second)
	if _, ok := e.fd.Lookup("/Thumbs.db"); ok {
		t.Fatal("junk uploaded")
	}
	if _, err := e.fs.Stat(ctx, "/Thumbs.db"); err != nil {
		t.Fatalf("junk not kept locally: %v", err)
	}
	// share modes
	h1, err := e.fs.Open(ctx, "/new.txt", vfs.OpenFlags{Access: vfs.AccessRead | vfs.AccessWrite, Share: vfs.ShareRead})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fs.Open(ctx, "/new.txt", vfs.OpenFlags{Access: vfs.AccessWrite, Share: vfs.ShareAll}); vfs.KindOf(err) != vfs.KindSharingViolation {
		t.Fatalf("second writer: %v", err)
	}
	h2, err := e.fs.Open(ctx, "/new.txt", vfs.OpenFlags{Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		t.Fatalf("reader beside a writer that shares read: %v", err)
	}
	_ = h2.Close()
	_ = h1.Close()
	// rename over an existing file (Office save pattern)
	if err := e.writeFile("/~WRL0001.tmp", []byte("saved by an app"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Rename(ctx, "/~WRL0001.tmp", "/new.txt", true); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(10 * time.Second)
	if fi, ok := e.fd.Lookup("/new.txt"); !ok || string(fi.Data) != "saved by an app" {
		t.Fatalf("rename-over result on the drive: %q", fi.Data)
	}
}

func TestFirstListingIsBounded(t *testing.T) {
	e := newEnv(t)
	slow := e.fd.AddFolder(e.fd.RootID(), "Slow")
	for i := 0; i < 20; i++ {
		e.fd.AddFile(slow, fmt.Sprintf("s%02d", i), []byte("s"))
	}
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	if _, _, err := e.fs.ReadDir(ctx, "/", "", 0); err != nil {
		t.Fatal(err)
	}
	e.fd.SetLatency(2500 * time.Millisecond)
	t0 := time.Now()
	_, _, err := e.fs.ReadDir(ctx, "/Slow", "", 0)
	if vfs.KindOf(err) != vfs.KindTimeout || time.Since(t0) > 2300*time.Millisecond {
		t.Fatalf("slow first listing: %v after %v", err, time.Since(t0))
	}
	e.fd.SetLatency(0)
	waitFor(t, 5*time.Second, "the listing to finish in the background", func() bool {
		infos, _, err := e.fs.ReadDir(ctx, "/Slow", "", 0)
		return err == nil && len(infos) == 20
	})
	// paging with markers
	page, next, err := e.fs.ReadDir(ctx, "/Slow", "", 8)
	if err != nil || len(page) != 8 || next != "s07" {
		t.Fatalf("page 1: %d %q %v", len(page), next, err)
	}
	page, next, _ = e.fs.ReadDir(ctx, "/Slow", next, 100)
	if len(page) != 12 || next != "" || page[0].Name != "s08" {
		t.Fatalf("page 2: %d %q", len(page), next)
	}
}
