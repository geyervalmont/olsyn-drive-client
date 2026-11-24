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

// A 429 is waited out (Retry-After), shared by every request of the
// client, and never reaches the caller while the drive lets it through
// in the end.
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
