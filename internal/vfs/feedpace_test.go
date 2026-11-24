package vfs_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

// bulkRun is what one bulk copy measured.
type bulkRun struct {
	took        time.Duration
	changes     int // GET /api/v1/changes calls the drive answered or refused
	changes429  int
	perMinute   float64
	uploadStart int
	// requests of the upload routes, and of pre-checks and folder
	// making, that waited because the drive had asked to wait
	uploadsHeld, otherHeld int64
	listings              int // children listings
}

// bulkCopy writes files small files (100 to a folder) through the file
// system, as Explorer copies a folder tree, against a drive that enforces
// the real feed limit (60 changes calls a minute, Retry-After 60), and waits
// until all are on the drive. With stream, the SSE stream is on and wakes
// the client for every change, its own uploads included.
func bulkCopy(t *testing.T, files int, stream bool) bulkRun {
	t.Helper()
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10,
		ChangesRate: 60, ChangesRetryAfter: time.Minute})
	e.stream = stream
	e.poll = 30 * time.Second
	e.start()
	defer e.stop()
	mt := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	e.mkdir("/Copy")
	t0 := time.Now()
	for i := range files {
		if i%100 == 0 {
			e.mkdir(fmt.Sprintf("/Copy/Folder-%02d", i/100))
		}
		p := fmt.Sprintf("/Copy/Folder-%02d/drawing-%03d.txt", i/100, i%100)
		if err := e.writeFile(p, payload(200+i%100, byte(i)), mt); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for e.fs.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.fs.Pending(); n > 0 {
		took := time.Since(t0)
		t.Fatalf("stream=%v: %d of %d files still to send after %v: %d changes calls (%.1f a minute), %d refused (429), %d upload starts",
			stream, n, files, took.Round(time.Second), e.fd.Count("changes"), float64(e.fd.Count("changes"))/took.Minutes(),
			e.fd.Count("changes.429"), e.fd.Count("upload.start"))
	}
	r := bulkRun{took: time.Since(t0), changes: e.fd.Count("changes"), changes429: e.fd.Count("changes.429"),
		uploadStart: e.fd.Count("upload.start"), uploadsHeld: e.api.PaceStats(driveapi.ClassUpload).Held,
		otherHeld: e.api.PaceStats(driveapi.ClassOther).Held + e.api.PaceStats(driveapi.ClassCheck).Held,
		listings: e.fd.Count("children")}
	r.perMinute = float64(r.changes) / max(r.took.Minutes(), 1.0/60)
	if n := len(e.fs.Held()); n != 0 {
		t.Fatalf("%d changes held: %+v", n, e.fs.Held()[0])
	}
	return r
}

// TestBulkCopyWithStreamNoise: the SSE stream wakes the client for every
// change during a bulk copy (most of them its own uploads). The feed is
// read at a pace the drive allows, a wait the feed is asked for never holds
// an upload, and the copy is about as fast as without the stream.
func TestBulkCopyWithStreamNoise(t *testing.T) {
	if testing.Short() {
		t.Skip("6,000 files")
	}
	const files = 3000
	base := bulkCopy(t, files, false)
	t.Logf("no stream: %v, %d changes calls", base.took.Round(time.Millisecond), base.changes)
	noisy := bulkCopy(t, files, true)
	for _, r := range []struct {
		name string
		r    bulkRun
	}{{"no stream", base}, {"stream", noisy}} {
		t.Logf("%s: %d files in %v (%.0f files/s), %d changes calls (%.1f a minute), %d refused (429), %d upload starts, %d upload requests held, %d listings",
			r.name, files, r.r.took.Round(time.Millisecond), float64(files)/r.r.took.Seconds(), r.r.changes, r.r.perMinute,
			r.r.changes429, r.r.uploadStart, r.r.uploadsHeld, r.r.listings)
	}
	if noisy.changes429 != 0 || base.changes429 != 0 {
		t.Fatalf("the feed was refused %d times (429)", noisy.changes429+base.changes429)
	}
	if n := noisy.uploadsHeld + noisy.otherHeld + base.uploadsHeld + base.otherHeld; n != 0 {
		t.Fatalf("%d upload, pre-check or folder requests waited for a 429", n)
	}
	if noisy.listings > base.listings+2 {
		t.Fatalf("%d listings with the stream, %d without: this PC's own changes made it list again", noisy.listings, base.listings)
	}
	if noisy.perMinute > driveapi.ChangesRateLimit {
		t.Fatalf("%.1f changes calls a minute", noisy.perMinute)
	}
	if limit := 1.2 * float64(base.took); float64(noisy.took) > limit && noisy.took-base.took > 2*time.Second {
		t.Fatalf("with the stream %v, without %v: more than 20%% slower", noisy.took, base.took)
	}
}

// TestOthersChangesReadAtAPace: someone else adds 50 files a second for 8
// seconds; every one wakes this PC through the stream. The feed is read
// at most once per interval (2 s), and the last change is read soon after
// it (a read always follows the last wake-up), without a 429.
func TestOthersChangesReadAtAPace(t *testing.T) {
	if testing.Short() {
		t.Skip("8 s of changes")
	}
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10,
		ChangesRate: 60, ChangesRetryAfter: time.Minute})
	e.stream = true
	e.poll = 30 * time.Second
	e.start()
	defer e.stop()
	waitFor(t, 5*time.Second, "the first cursor", func() bool { return e.fd.Count("changes") > 0 })
	folder := e.fd.AddFolder(e.fd.RootID(), "Shared")
	waitFor(t, 5*time.Second, "the folder", func() bool { return e.has(folder) })
	before := e.fd.Count("changes")
	t0 := time.Now()
	var last string
	n := 0
	for ; time.Since(t0) < 8*time.Second; n++ {
		last = e.fd.AddFile(folder, fmt.Sprintf("other-%04d.txt", n), []byte("x"))
		time.Sleep(20 * time.Millisecond)
	}
	lastAt := time.Now()
	waitFor(t, 10*time.Second, "the last change", func() bool { return e.has(last) })
	lag := time.Since(lastAt)
	calls := e.fd.Count("changes") - before
	t.Logf("%d changes by someone else in %v: %d changes calls, the last read %v after it, %d refused (429)",
		n, lastAt.Sub(t0).Round(time.Millisecond), calls, lag.Round(time.Millisecond), e.fd.Count("changes.429"))
	if e.fd.Count("changes.429") != 0 {
		t.Fatal("the feed was refused (429)")
	}
	// 8 s, at least 2 s apart, plus the trailing read
	if calls > 7 {
		t.Fatalf("%d changes calls for 8 s of wake-ups", calls)
	}
	if lag > 4*time.Second {
		t.Fatalf("the last change was read %v after it", lag)
	}
}
