package driveapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSplitCheckKeepsBothLimits(t *testing.T) {
	// 2,500 files in 2,500 folders: the folder limit cuts every 200
	var wide []CheckFile
	for i := range 2500 {
		wide = append(wide, CheckFile{ParentID: fmt.Sprint("f", i), Name: "a"})
	}
	// 2,500 files in 5 folders: the file limit cuts every 1,000
	var deep []CheckFile
	for i := range 2500 {
		deep = append(deep, CheckFile{ParentID: fmt.Sprint("f", i/500), Name: fmt.Sprint(i)})
	}
	for _, tc := range []struct {
		name  string
		files []CheckFile
		calls int
	}{{"wide", wide, 13}, {"deep", deep, 3}, {"empty", nil, 0}} {
		groups := SplitCheck(tc.files)
		n := 0
		for _, g := range groups {
			parents := map[string]bool{}
			for _, f := range g {
				parents[f.ParentID] = true
			}
			if len(g) == 0 || len(g) > CheckMaxFiles || len(parents) > CheckMaxFolders {
				t.Fatalf("%s: a group of %d files in %d folders", tc.name, len(g), len(parents))
			}
			n += len(g)
		}
		if len(groups) != tc.calls || n != len(tc.files) {
			t.Fatalf("%s: %d groups holding %d files", tc.name, len(groups), n)
		}
	}
}

// A 429 is waited out (Retry-After), shared by every request of its route
// class, and never reaches the caller while the drive lets it through in
// the end.
func TestRateLimitedIsWaitedOutAndShared(t *testing.T) {
	var calls, limited atomic.Int32
	var mu sync.Mutex
	var openAt time.Time
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		if openAt.IsZero() {
			openAt = time.Now().Add(time.Second)
		}
		closed := time.Now().Before(openAt)
		mu.Unlock()
		if closed {
			limited.Add(1)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limited","message":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"state":"absent"}]}`))
	})
	c, _ := newTestClient(t, h)
	c.check = nil // this test is about 429s, not the client's own pace
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.CheckUploads(ctx, []CheckFile{{ParentID: "p", Name: "n", Size: 1}}, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a 429 reached the caller: %v", err)
		}
	}
	took := time.Since(start)
	if took < time.Second {
		t.Fatalf("did not wait for Retry-After: %v", took)
	}
	// one wait holds everyone: no worker keeps hitting the drive meanwhile
	if l := limited.Load(); l > 8 {
		t.Fatalf("%d calls refused for 8 callers: the wait was not shared", l)
	}
	t.Logf("8 parallel callers: %d calls, %d refused (429), all through in %v", calls.Load(), limited.Load(), took.Round(time.Millisecond))
}

// After rateRetries 429s in a row the 429 is returned, as retryable, with
// the drive's Retry-After for the caller's own wait.
func TestRateLimitedGivesUpRetryable(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, _ := newTestClient(t, h)
	c.rateRetries = 2
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.GetNode(ctx, "x")
	if !IsRetryable(err) || IsPermanent(err) || RetryAfter(err) != time.Second {
		t.Fatalf("want a retryable 429, got %v", err)
	}
	if n, d := calls.Load(), time.Since(start); n != 3 || d < 2*time.Second {
		t.Fatalf("%d calls in %v: want 3 calls, 1 s apart", n, d)
	}
}

func TestBucketPaces(t *testing.T) {
	b := newBucket(600, 2) // one every 100 ms after a burst of 2
	ctx := context.Background()
	start := time.Now()
	for range 6 {
		if err := b.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// 2 at once, then 4 × 100 ms
	if d := time.Since(start); d < 350*time.Millisecond || d > 2*time.Second {
		t.Fatalf("6 takes in %v", d)
	}
	var none *bucket
	if err := none.take(ctx); err != nil {
		t.Fatal(err)
	}
}

// A 429 on the changes feed holds the feed only: upload routes, listings
// and the heartbeat go on at once, and none of their requests is held.
func TestRateLimitHoldsOnlyItsClass(t *testing.T) {
	var feedCalls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/changes":
			if feedCalls.Add(1) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limited"}`))
				return
			}
			_, _ = w.Write([]byte(`{"changes":[],"next":7,"has_more":false,"reset":null}`))
		case "/api/v1/uploads":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"upload_id":"u1","method":"single","part_size":1,"part_count":1}`))
		case "/api/mount/v1/heartbeat":
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"id":"n1","kind":"folder","name":"x"}`))
		}
	})
	c, _ := newTestClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	feedDone := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Changes(ctx, 1, 0, "")
		feedDone <- err
	}()
	// the feed's 429 is in
	for feedCalls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	for range 5 {
		if _, err := c.StartUpload(ctx, StartUpload{ParentID: "p", Name: "a.txt", Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.GetNode(ctx, "n1"); err != nil {
			t.Fatal(err)
		}
		if err := c.SendHeartbeat(ctx, Heartbeat{State: "mounted"}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("other routes took %v while the feed waited", d)
	}
	if err := <-feedDone; err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("the feed did not wait for Retry-After: %v", d)
	}
	for _, cl := range []Class{ClassUpload, ClassListing, ClassOther} {
		if s := c.PaceStats(cl); s.Held != 0 || s.Limited != 0 {
			t.Fatalf("%s: %+v", cl, s)
		}
	}
	if s := c.PaceStats(ClassChanges); s.Limited != 1 || s.Held != 1 || s.Limit != ChangesRateLimit {
		t.Fatalf("changes: %+v", s)
	}
}

// A heartbeat is never held: a 429 comes back at once (the next beat is
// on its own schedule), even while its class is waiting.
func TestHeartbeatDoesNotWait(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, _ := newTestClient(t, h)
	start := time.Now()
	for range 2 {
		err := c.SendHeartbeat(context.Background(), Heartbeat{State: "mounted"})
		if !IsWait(err) || RetryAfter(err) != time.Minute {
			t.Fatalf("want the 429, got %v", err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("heartbeats waited %v", d)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         Class
	}{
		{"GET", "/api/v1/changes", ClassChanges},
		{"POST", "/api/v1/uploads/check", ClassCheck},
		{"POST", "/api/v1/uploads", ClassUpload},
		{"POST", "/api/v1/uploads/u1/complete", ClassUpload},
		{"POST", "/api/v1/uploads/u1/parts", ClassUpload},
		{"GET", "/api/v1/nodes/n1/children", ClassListing},
		{"GET", "/api/v1/resolve", ClassListing},
		{"GET", "/api/mount/v1/nodes/n1/content", ClassDownload},
		{"POST", "/api/v1/nodes/n1/folders", ClassOther},
		{"PATCH", "/api/v1/nodes/n1", ClassOther},
		{"POST", "/api/mount/v1/heartbeat", ClassOther},
		{"GET", "/api/v1/stream", ClassOther},
	} {
		if got := classify(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}

// A class never makes more than 80% of its known limit in a window.
func TestGuardKeepsUnderKnownLimit(t *testing.T) {
	p := newPacer(Limits{ClassChanges: 10})
	p.window = 400 * time.Millisecond
	ctx := context.Background()
	start := time.Now()
	for range 8 { // 80% of 10
		if err := p.wait(ctx, ClassChanges); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("8 calls under a limit of 10 waited %v", d)
	}
	if err := p.wait(ctx, ClassChanges); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 350*time.Millisecond {
		t.Fatalf("the 9th call went after %v, inside the window", d)
	}
	if s := p.stats(ClassChanges); s.Calls != 9 || s.Paced != 1 || s.Held != 0 {
		t.Fatalf("%+v", s)
	}
	// another class is not slowed by it
	t0 := time.Now()
	for range 50 {
		if err := p.wait(ctx, ClassDownload); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("downloads waited %v", d)
	}
}

// A class with no known limit learns one from its first 429 (what it made
// in the window before, the window from Retry-After) and keeps under 80% of
// it; a 429 after a few calls teaches nothing.
func TestUnknownLimitIsLearned(t *testing.T) {
	p := newPacer(nil)
	ctx := context.Background()
	for range 3 {
		_ = p.wait(ctx, ClassListing)
	}
	p.hold(ClassListing, 0, time.Second)
	if s := p.stats(ClassListing); s.Limit != 0 || s.Limited != 1 {
		t.Fatalf("learned from 3 calls: %+v", s)
	}
	for range 22 {
		_ = p.wait(ctx, ClassListing)
	}
	p.hold(ClassListing, 0, 300*time.Millisecond) // a window of at least 1 s
	if s := p.stats(ClassListing); s.Limit != 25 || !s.Learned || s.Window != time.Second {
		t.Fatalf("want a learned limit of 25 a second: %+v", s)
	}
	// 25 in the window, the guard is 20: the next call waits for the window
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := p.wait(short, ClassListing); err == nil {
		t.Fatal("a call went over the learned limit")
	}
	if err := p.wait(ctx, ClassDownload); err != nil {
		t.Fatal(err)
	}
	if s := p.stats(ClassDownload); s.Limit != 0 || s.Paced != 0 {
		t.Fatalf("download: %+v", s)
	}
	// the window passes: calls go again
	time.Sleep(time.Second)
	t0 := time.Now()
	if err := p.wait(ctx, ClassListing); err != nil || time.Since(t0) > 100*time.Millisecond {
		t.Fatalf("after the window: %v, %v", err, time.Since(t0))
	}
}
