package transfer_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// wideTree writes 12,000 small files in 600 leaf folders (6 × 10 × 10):
// the first 300 folders hold 2 files each, so the pre-check's limit of 200
// folders a call is what cuts there, and the other 300 hold 38 each, where
// the limit of 1,000 files a call cuts.
func wideTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Tower")
	n := 0
	for i := range 600 {
		dir := filepath.Join(root, fmt.Sprintf("L%d", i/100), fmt.Sprintf("M%d", i/10%10), fmt.Sprintf("F%d", i%10))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := 2
		if i >= 300 {
			files = 38
		}
		for k := range files {
			n++
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("sheet-%02d.txt", k)), []byte(fmt.Sprintf("file %d of the wide tree\n", n)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n != 12000 {
		t.Fatalf("wrote %d files", n)
	}
	return root
}

// TestSendWideTreeStaysUnderCheckLimits sends 12,000 files across 600
// folders to a drive that enforces the production pre-check limits (1,000
// files and 200 folders a call, 120 calls a minute), then sends the same
// folder again (everything is already there), then again against a drive
// whose rate limit is far tighter than the client's own pace: no call is
// ever refused for its size, 429s are waited out, nothing fails or is held.
func TestSendWideTreeStaysUnderCheckLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("12,000 files")
	}
	fd := fakedrive.New(fakedrive.Options{PartSize: partSize, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	newAPI := func(checkPerMinute int) *driveapi.Client {
		api, err := driveapi.New(driveapi.Options{BaseURL: fd.URL(), CheckPerMinute: checkPerMinute,
			Token:  func(context.Context) (string, error) { return fd.Token(), nil },
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return api
	}
	// every folder made costs 20 ms on the "network": made one at a time,
	// 667 folders would take over 13 s
	const folderLatency = 20 * time.Millisecond
	// busy: the time during which at least one folder was being made
	var mu sync.Mutex
	var inflight int
	var busySince time.Time
	var busy time.Duration
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/folders") {
			mu.Lock()
			if inflight == 0 {
				busySince = time.Now()
			}
			inflight++
			mu.Unlock()
			time.Sleep(folderLatency)
			mu.Lock()
			if inflight--; inflight == 0 {
				busy += time.Since(busySince)
			}
			mu.Unlock()
		}
		return false
	})
	src := wideTree(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	run := func(name string, api *driveapi.Client) *outcomes {
		t.Helper()
		var o outcomes
		eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		s := &transfer.Sender{API: api, Engine: eng, Backoff: fastBackoff, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		checks, refused, limited := fd.Count("check"), fd.Count("check.422"), fd.Count("check.429")
		start := time.Now()
		if err := s.Send(ctx, transfer.SendSpec{BatchID: "b_" + name, Sources: []string{src}, ParentID: fd.RootID(), ParentPath: "/", Files: 8}, o.report()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Logf("%s: 12,000 files in %v: %d pre-check calls, %d refused for size (422), %d asked to wait (429)",
			name, time.Since(start).Round(time.Millisecond), fd.Count("check")-checks, fd.Count("check.422")-refused, fd.Count("check.429")-limited)
		if n := fd.Count("check.422") - refused; n != 0 {
			t.Fatalf("%s: %d pre-checks refused for their size", name, n)
		}
		if o.found != 12000 {
			t.Fatalf("%s: found %d files", name, o.found)
		}
		return &o
	}

	o := run("first send", newAPI(0))
	if n := o.count(transfer.SendDone); n != 12000 {
		t.Fatalf("first send: %d done; codes %v", n, sampleCodes(o))
	}
	folders := fd.Count("folder.create")
	serial := time.Duration(folders) * folderLatency
	mu.Lock()
	made := busy
	mu.Unlock()
	t.Logf("first send: %d folders made in %v of folder calls (one at a time: at least %v)", folders, made.Round(time.Millisecond), serial)
	if folders != 1+6+60+600 {
		t.Fatalf("%d folders made", folders)
	}
	if made > serial/2 {
		t.Fatalf("folders took %v, one at a time would be %v", made, serial)
	}

	before := fd.Count("check")
	o = run("resend", newAPI(0))
	if n := o.count(transfer.SendSkipped); n != 12000 {
		t.Fatalf("resend: %d skipped as already there; codes %v", n, sampleCodes(o))
	}
	if calls := fd.Count("check") - before; calls > 60 || calls < 12 {
		t.Fatalf("resend: %d pre-check calls", calls)
	}
	if n := fd.Count("upload.start"); n != 12000 {
		t.Fatalf("%d uploads started over both runs", n)
	}

	// a drive allowing 3 pre-checks every 2 s, and a client that does not
	// pace itself: the drive's 429s are what slow it down
	fd.SetRateLimits(3, 0, 2*time.Second)
	limited := fd.Count("check.429")
	o = run("resend, rate limited", newAPI(-1))
	if n := o.count(transfer.SendSkipped); n != 12000 {
		t.Fatalf("rate limited resend: %d skipped; codes %v", n, sampleCodes(o))
	}
	if fd.Count("check.429") == limited {
		t.Fatal("the drive never asked to wait: the test proves nothing")
	}
}

func sampleCodes(o *outcomes) map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]int{}
	for p, c := range o.codes {
		out[o.by[p]+":"+c]++
	}
	return out
}

// TestSendDateOutOfRange: the drive refuses a modified time out of its
// range at upload start (nothing created); the file is sent once more
// without it and its result says the date was not kept.
func TestSendDateOutOfRange(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	s := &transfer.Sender{API: api, Engine: eng, Backoff: fastBackoff}
	src := filepath.Join(t.TempDir(), "Dates")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	ok := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	for name, mt := range map[string]time.Time{"future.txt": future, "fine.txt": ok} {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	var o outcomes
	if err := s.Send(ctxT(t), transfer.SendSpec{BatchID: "b_dates", Sources: []string{src}, ParentID: fd.RootID(), ParentPath: "/"}, o.report()); err != nil {
		t.Fatal(err)
	}
	if o.by["/Dates/future.txt"] != transfer.SendDone || o.codes["/Dates/future.txt"] != transfer.NoteDateNotKept {
		t.Fatalf("future.txt: %s %s", o.by["/Dates/future.txt"], o.codes["/Dates/future.txt"])
	}
	if o.by["/Dates/fine.txt"] != transfer.SendDone || o.codes["/Dates/fine.txt"] != "" {
		t.Fatalf("fine.txt: %s %s", o.by["/Dates/fine.txt"], o.codes["/Dates/fine.txt"])
	}
	if n := fd.Count("upload.start.422.modified_at"); n != 1 {
		t.Fatalf("%d starts refused for the date", n)
	}
	f, found := fd.Lookup("/Dates/future.txt")
	if !found || f.ModifiedAt != nil || string(f.Data) != "content of future.txt" {
		t.Fatalf("future.txt on the drive: %v %+v", found, f.ModifiedAt)
	}
	if f, found := fd.Lookup("/Dates/fine.txt"); !found || f.ModifiedAt == nil || !f.ModifiedAt.Equal(ok) {
		t.Fatal("fine.txt lost its date")
	}
}
