package vfs_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// soon releases verified uploads after 150 ms and keeps files read in the
// last 400 ms; the rules run every 50 ms.
var soon = cache.ReleasePolicy{AfterUpload: 150 * time.Millisecond, KeepOpened: 400 * time.Millisecond, VerifyWait: time.Hour}

// claimedOpts: files over 128 KiB are "claimed" at upload and verified by
// the drive's checksum pass later.
var claimedOpts = fakedrive.Options{PartSize: part, SingleMax: 100 << 10, VerifyMax: 128 << 10}

func (e *env) releasing(p cache.ReleasePolicy) *env {
	e.release, e.releaseEvery = p, 50*time.Millisecond
	return e
}

func (e *env) stat(path string) vfs.Info {
	e.t.Helper()
	in, err := e.fs.Stat(ctxT(e.t), path)
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

func (e *env) cached(path string) bool {
	e.t.Helper()
	return !e.stat(path).Offline
}

// A file the drive verified at upload leaves this PC after the grace
// period: it shows as online-only again, the cache is empty, and it still
// reads (from the drive).
func TestVerifiedUploadIsReleasedAfterGrace(t *testing.T) {
	e := newEnv(t).releasing(soon).start()
	defer e.stop()
	data := payload(3*part, 4)
	if err := e.writeFile("/report.pdf", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if fi, _ := e.fd.Lookup("/report.pdf"); fi.ChecksumState != driveapi.ChecksumVerified {
		t.Fatalf("checksum state %q", fi.ChecksumState)
	}
	if u := e.fs.CacheUsage(); u.Uploaded != int64(len(data)) || u.Unsent != 0 {
		t.Fatalf("usage right after the upload %+v", u)
	}
	waitFor(t, 5*time.Second, "the upload to be released", func() bool { return !e.cached("/report.pdf") })
	if u := e.fs.CacheUsage(); u.Used != 0 {
		t.Fatalf("cache still holds %+v", u)
	}
	got, err := e.readFile("/report.pdf")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after release: %v", err)
	}
	// content fetched for reading is ordinary cached content: it stays
	// (evicted oldest first only when the cache is over its cap)
	time.Sleep(300 * time.Millisecond)
	if !e.cached("/report.pdf") {
		t.Fatal("content read from the drive was released early")
	}
}

// A large upload the drive only "claimed" stays until the drive's
// checksum pass verifies it through the changes feed; then it goes.
func TestClaimedUploadStaysUntilVerified(t *testing.T) {
	e := newEnvWith(t, claimedOpts).releasing(soon).start()
	defer e.stop()
	data := payload(5*part+9, 5)
	if err := e.writeFile("/model.rvt", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if fi, _ := e.fd.Lookup("/model.rvt"); fi.ChecksumState != driveapi.ChecksumClaimed {
		t.Fatalf("checksum state %q", fi.ChecksumState)
	}
	time.Sleep(500 * time.Millisecond) // past the grace period
	if !e.cached("/model.rvt") {
		t.Fatal("released before the drive verified it")
	}
	if u := e.fs.CacheUsage(); u.Uploaded != int64(len(data)) {
		t.Fatalf("usage while claimed %+v", u)
	}
	if n := e.fd.RunChecksumPass(); n != 1 {
		t.Fatalf("pass checked %d", n)
	}
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "release after verification", func() bool { return !e.cached("/model.rvt") })
}

// A drive that never verifies (or has no checksum states at all): the only
// copy the drive has not confirmed is never released early, and "free up
// space" leaves it too; after the verify wait it is ordinary cached content.
func TestUnconfirmedUploadIsKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts fakedrive.Options
	}{
		{"never verifies", fakedrive.Options{PartSize: part, SingleMax: 100 << 10, VerifyMax: 128 << 10, NeverVerify: true}},
		{"no checksum state", fakedrive.Options{PartSize: part, SingleMax: 100 << 10, NoChecksumState: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := soon
			p.VerifyWait = 300 * time.Millisecond
			e := newEnvWith(t, tc.opts).releasing(p).start()
			defer e.stop()
			data := payload(5*part, 6)
			if err := e.writeFile("/a.bin", data, time.Time{}); err != nil {
				t.Fatal(err)
			}
			e.waitSynced(20 * time.Second)
			e.fd.RunChecksumPass()
			e.fs.Refresh()
			time.Sleep(600 * time.Millisecond)
			if !e.cached("/a.bin") {
				t.Fatal("an unconfirmed upload was released")
			}
			if n, _ := e.fs.FreeCache(); n != 0 || !e.cached("/a.bin") {
				t.Fatal("free up space dropped an unconfirmed upload")
			}
			if u := e.fs.CacheUsage(); u.Opened != int64(len(data)) || u.Uploaded != 0 {
				t.Fatalf("usage after the verify wait %+v", u)
			}
		})
	}
}

// Open files, files read recently and kept-offline files stay.
func TestReleaseSkipsOpenReadAndKeptFiles(t *testing.T) {
	e := newEnv(t).releasing(soon)
	e.keepOffline = func(p string) bool { return p == "/kept/b.bin" }
	e.start()
	defer e.stop()
	ctx := ctxT(t)
	e.mkdir("/kept")
	for _, p := range []string{"/open.bin", "/read.bin", "/kept/b.bin"} {
		if err := e.writeFile(p, payload(2*part, 7), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	// open one and keep reading the other from before the uploads finish
	// until after the grace period has passed
	h, err := e.fs.Open(ctx, "/open.bin", vfs.OpenFlags{Access: vfs.AccessRead, Share: vfs.ShareAll})
	if err != nil {
		t.Fatal(err)
	}
	stop, readErr := make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(readErr)
		for {
			if _, err := e.readFile("/read.bin"); err != nil {
				readErr <- err
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	e.waitSynced(20 * time.Second)
	time.Sleep(400 * time.Millisecond)
	open, read, kept := e.cached("/open.bin"), e.cached("/read.bin"), e.cached("/kept/b.bin")
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	if !open || !read || !kept {
		t.Fatalf("released: open %v, recently read %v, kept %v", !open, !read, !kept)
	}
	waitFor(t, 5*time.Second, "release once reading stopped", func() bool { return !e.cached("/read.bin") })
	_ = h.Close()
	waitFor(t, 5*time.Second, "release once closed", func() bool { return !e.cached("/open.bin") })
	time.Sleep(200 * time.Millisecond)
	if !e.cached("/kept/b.bin") {
		t.Fatal("a kept-offline file was released")
	}
	if n, _ := e.fs.FreeCache(); n != 0 || !e.cached("/kept/b.bin") {
		t.Fatal("free up space dropped a kept-offline file")
	}
}

// Crash safety: a claimed upload whose verification arrives after a crash
// and restart is still kept until then, and released after.
func TestReleaseAfterRestart(t *testing.T) {
	e := newEnvWith(t, claimedOpts).releasing(soon).start()
	data := payload(5*part, 8)
	if err := e.writeFile("/big.bin", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	e.kill()
	e.start()
	defer e.stop()
	time.Sleep(400 * time.Millisecond)
	if !e.cached("/big.bin") {
		t.Fatal("claimed upload released after a restart")
	}
	e.fd.RunChecksumPass()
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "release after verification", func() bool { return !e.cached("/big.bin") })
	if got, err := e.readFile("/big.bin"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read after release: %v", err)
	}
}

// Free up space releases opened content and verified uploads at once,
// whatever the timings; unsent content stays.
func TestFreeCacheReleasesWhatTheDriveHas(t *testing.T) {
	e := newEnv(t).releasing(cache.ReleasePolicy{AfterUpload: time.Hour, KeepOpened: time.Hour}).start()
	defer e.stop()
	id := e.fd.AddFile(e.fd.RootID(), "drive.bin", payload(3*part, 1))
	_ = id
	if _, err := e.readFile("/drive.bin"); err != nil {
		t.Fatal(err)
	}
	if err := e.writeFile("/mine.bin", payload(2*part, 2), time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	e.eng.Pause()
	if err := e.writeFile("/unsent.bin", payload(part, 3), time.Time{}); err != nil {
		t.Fatal(err)
	}
	n, b := e.fs.FreeCache()
	if n != 2 || b != 5*part {
		t.Fatalf("freed %d files, %d bytes", n, b)
	}
	if u := e.fs.CacheUsage(); u.Unsent != part || u.Used != part {
		t.Fatalf("after freeing %+v", u)
	}
	e.eng.Resume()
	e.waitSynced(20 * time.Second)
}
