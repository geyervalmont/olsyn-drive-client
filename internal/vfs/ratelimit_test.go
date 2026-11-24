package vfs_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

// TestBurstOfClosesUnderRateLimits saves 3,000 small files (Explorer
// copying a folder tree) against a drive that enforces the production
// pre-check limits and, beyond production, limits upload starts too: its
// 429s are waited out, nothing is held, nothing is refused for its size.
func TestBurstOfClosesUnderRateLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("3,000 files")
	}
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10,
		UploadStartRate: 100, RateWindow: time.Second}).start()
	defer e.stop()
	// the uploads wait until every file is saved, then go at once
	release := slowUploads(e.fd)
	mt := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	t0 := time.Now()
	for d := range 30 {
		dir := fmt.Sprintf("/Copy/Folder-%02d", d)
		if d == 0 {
			e.mkdir("/Copy")
		}
		e.mkdir(dir)
		for i := range 100 {
			if err := e.writeFile(fmt.Sprintf("%s/drawing-%03d.txt", dir, i), payload(200+i, byte(d+i)), mt); err != nil {
				t.Fatal(err)
			}
		}
	}
	written := time.Since(t0)
	release()
	e.waitSynced(3 * time.Minute)
	t.Logf("3,000 closes in %v, all on the drive %v later: %d upload starts, %d asked to wait (429), %d pre-checks, %d refused for size (422)",
		written.Round(time.Millisecond), (time.Since(t0) - written).Round(time.Millisecond), e.fd.Count("upload.start"), e.fd.Count("upload.start.429"),
		e.fd.Count("check"), e.fd.Count("check.422"))
	if n := len(e.fs.Held()); n != 0 {
		t.Fatalf("%d changes held: %+v", n, e.fs.Held()[0])
	}
	if e.fd.Count("check.422") != 0 || e.fd.Count("check.429") != 0 {
		t.Fatal("a pre-check was refused")
	}
	if e.fd.Count("upload.start.429") == 0 {
		t.Fatal("the drive never asked to wait: the test proves nothing")
	}
	for d := 0; d < 30; d += 7 {
		for i := 0; i < 100; i += 33 {
			p := fmt.Sprintf("/Copy/Folder-%02d/drawing-%03d.txt", d, i)
			fi, ok := e.fd.Lookup(p)
			if !ok || string(fi.Data) != string(payload(200+i, byte(d+i))) {
				t.Fatalf("%s missing or wrong on the drive", p)
			}
		}
	}
}

// TestSaveWithDateOutOfRange: a file whose date the drive refuses (out of
// its range) is sent without it, not held.
func TestSaveWithDateOutOfRange(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	e.mkdir("/Dates")
	if err := e.writeFile("/Dates/future.txt", []byte("dated 2099"), time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if n := len(e.fs.Held()); n != 0 {
		t.Fatalf("held: %+v", e.fs.Held()[0])
	}
	fi, ok := e.fd.Lookup("/Dates/future.txt")
	if !ok || string(fi.Data) != "dated 2099" || fi.ModifiedAt != nil {
		t.Fatalf("on the drive: %v %v", ok, fi.ModifiedAt)
	}
	if n := e.fd.Count("upload.start.422.modified_at"); n != 1 {
		t.Fatalf("%d starts refused for the date", n)
	}
}
