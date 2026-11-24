package driveapi

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pacing: staying under the drive's rate limits, and waiting when it asks.
//
// Requests fall into route classes (Class). Each class has its own pace:
//
//   - A 429 is "wait", never a failure: send waits as the drive asked and
//     tries again, a bounded number of times, before it hands the 429 to the
//     caller (which classifies it as retryable). The wait holds every
//     request of that class until the time is up, so parallel workers do not
//     each hit the limit in turn, and each waiter adds its own small random
//     delay so they do not all come back at the same instant. It never holds
//     another class: the changes feed being asked to wait does not stop
//     uploads, and only a 429 on an upload route holds upload workers.
//   - A class whose limit is known (Limits) never makes more than
//     guardShare of it in any minute: calls beyond that wait for a slot.
//   - A class whose limit is unknown learns one from its first 429: the
//     calls it made in the window before, the window taken from Retry-After
//     (between 1 s and a minute), and keeps under guardShare of that from
//     then on. A later 429 can only lower the rate.
//
// Uploads' part PUTs go to the storage host on presigned URLs, not through
// send: no API pace ever delays them.

// Class is a group of the drive's routes paced together.
type Class int

// The route classes.
const (
	ClassOther    Class = iota // everything else: heartbeat, folders, renames, trash, sign-in, stream, …
	ClassChanges               // GET /api/v1/changes
	ClassCheck                 // POST /api/v1/uploads/check
	ClassUpload                // /api/v1/uploads… : start, part URLs, complete, abort, status
	ClassListing               // GET /api/v1/nodes/…, GET /api/v1/resolve
	ClassDownload              // GET /api/mount/v1/nodes/{id}/content
	numClasses
)

var classNames = [numClasses]string{"other", "changes", "uploads-check", "uploads", "listing", "download"}

func (c Class) String() string {
	if c < 0 || c >= numClasses {
		return "other"
	}
	return classNames[c]
}

// classify names the class of a request.
func classify(method, path string) Class {
	switch {
	case path == "/api/v1/changes":
		return ClassChanges
	case path == "/api/v1/uploads/check":
		return ClassCheck
	case path == "/api/v1/uploads" || strings.HasPrefix(path, "/api/v1/uploads/"):
		return ClassUpload
	case strings.HasPrefix(path, "/api/mount/v1/nodes/") && strings.HasSuffix(path, "/content"):
		return ClassDownload
	case method == http.MethodGet && (strings.HasPrefix(path, "/api/v1/nodes/") || path == "/api/v1/resolve"):
		return ClassListing
	}
	return ClassOther
}

// Limits are the drive's known limits, in calls a minute per credential, by
// class; a class not named has no known limit.
type Limits map[Class]int

// DefaultLimits are the documented limits of the drive.
func DefaultLimits() Limits {
	return Limits{ClassChanges: ChangesRateLimit, ClassCheck: CheckRateLimit}
}

const (
	// rateRetries is how many 429 answers one call waits out before it
	// returns the 429 to its caller.
	rateRetries = 8
	// rateWaitMax caps one wait, whatever Retry-After says.
	rateWaitMax = 2 * time.Minute
	// rateWaitMin is the wait when a 429 comes without Retry-After (doubled
	// on each further 429 of the same call).
	rateWaitMin = time.Second
	// guardShare is the share of a limit a class may use, in percent.
	guardShare = 80
	// paceWindow is the window of the limits.
	paceWindow = time.Minute
	// maxRecent bounds the call times a class remembers.
	maxRecent = 4096
	// learnMin is the fewest calls in a window a 429 must follow to teach a
	// class a limit: a 429 after fewer is a burst or overload limit, not a
	// count a minute, and is only waited out.
	learnMin = 20
)

// ClassStats are what one class's pacing has done.
type ClassStats struct {
	Calls   int64 // requests sent
	Limited int64 // 429 answers
	// Held counts requests that waited because the drive had asked this
	// class to wait (a 429 on this class).
	Held int64
	// Paced counts requests that waited for a slot under the class's guard.
	Paced int64
	// Limit is the known or learned limit (0: unknown) for each Window;
	// Learned says it came from a 429.
	Limit   int
	Window  time.Duration
	Learned bool
}

// classPace is one class's state.
type classPace struct {
	mu      sync.Mutex
	until   time.Time   // held until, after a 429
	recent  []time.Time // when recent calls were sent, oldest first
	limit   int
	window  time.Duration // of a learned limit (0: the pacer's)
	learned bool
	stats   ClassStats
}

// pacer holds the classes' paces.
type pacer struct {
	classes [numClasses]classPace
	window  time.Duration // paceWindow; tests shorten it
}

func newPacer(limits Limits) *pacer {
	p := &pacer{window: paceWindow}
	for c, n := range limits {
		if c >= 0 && c < numClasses && n > 0 {
			p.classes[c].limit = n
		}
	}
	return p
}

func (p *pacer) class(c Class) *classPace {
	if c < 0 || c >= numClasses {
		c = ClassOther
	}
	return &p.classes[c]
}

// hold makes every request of class c wait until d from now (or longer, if
// an earlier 429 asked for more). A class without a known limit learns one
// (see learn).
func (p *pacer) hold(c Class, d, retryAfter time.Duration) {
	cp := p.class(c)
	now := time.Now()
	t := now.Add(d)
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.stats.Limited++
	if t.After(cp.until) {
		cp.until = t
	}
	if cp.limit == 0 || cp.learned {
		cp.learn(now, min(max(retryAfter, time.Second), p.window))
	}
}

// learn takes the calls made in the window before now (the one refused
// included) as the class's limit for that window, when there were at least
// learnMin and the rate is lower than one learned before. A fixed-window
// limit answers Retry-After with the rest of its window, so the window is
// at least that long. cp.mu is held.
func (cp *classPace) learn(now time.Time, window time.Duration) {
	n := 0
	for _, t := range cp.recent {
		if now.Sub(t) < window {
			n++
		}
	}
	if n < learnMin {
		return
	}
	rate := float64(n) / window.Seconds()
	if cp.limit == 0 || rate < float64(cp.limit)/cp.window.Seconds() {
		cp.limit, cp.window, cp.learned = n, window, true
	}
}

// wait returns once request of class c may go: the drive's wait for the
// class is over (plus this caller's jitter) and the class has a slot under
// its guard. It records the call. It fails only when ctx ends.
func (p *pacer) wait(ctx context.Context, c Class) error {
	cp := p.class(c)
	held, paced := false, false
	for {
		cp.mu.Lock()
		now := time.Now()
		var d time.Duration
		if hold := cp.until.Sub(now); hold > 0 {
			held = true
			d = hold + spread(hold)
		} else if guard := cp.guard(); guard > 0 {
			window := cp.windowOr(p.window)
			cp.prune(now, p.window)
			if k := countSince(cp.recent, now.Add(-window)); k >= guard {
				paced = true
				d = cp.recent[len(cp.recent)-guard].Add(window).Sub(now) + time.Millisecond
			}
		}
		if d <= 0 {
			cp.record(now)
			if held {
				cp.stats.Held++
			}
			if paced {
				cp.stats.Paced++
			}
			cp.mu.Unlock()
			return nil
		}
		cp.mu.Unlock()
		if err := sleepCtx(ctx, d); err != nil {
			return err
		}
	}
}

// note records a call of class c that does not wait.
func (p *pacer) note(c Class) {
	cp := p.class(c)
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.record(time.Now())
}

// record notes a call sent at now. cp.mu is held.
func (cp *classPace) record(now time.Time) {
	cp.recent = append(cp.recent, now)
	if len(cp.recent) > maxRecent {
		cp.recent = append(cp.recent[:0], cp.recent[len(cp.recent)-maxRecent:]...)
	}
	cp.stats.Calls++
}

// windowOr is the window of the class's limit: a learned one's, else def.
func (cp *classPace) windowOr(def time.Duration) time.Duration {
	if cp.window > 0 {
		return cp.window
	}
	return def
}

// countSince counts the times (oldest first) at or after since.
func countSince(ts []time.Time, since time.Time) int {
	i := len(ts)
	for i > 0 && !ts[i-1].Before(since) {
		i--
	}
	return len(ts) - i
}

// guard is how many calls the class may make in a window (0: no guard).
func (cp *classPace) guard() int {
	if cp.limit <= 0 {
		return 0
	}
	return max(cp.limit*guardShare/100, 1)
}

// prune drops call times older than the window.
func (cp *classPace) prune(now time.Time, window time.Duration) {
	i := 0
	for i < len(cp.recent) && now.Sub(cp.recent[i]) >= window {
		i++
	}
	if i > 0 {
		cp.recent = append(cp.recent[:0], cp.recent[i:]...)
	}
}

func (p *pacer) stats(c Class) ClassStats {
	cp := p.class(c)
	cp.mu.Lock()
	defer cp.mu.Unlock()
	s := cp.stats
	s.Limit, s.Learned, s.Window = cp.limit, cp.learned, cp.windowOr(p.window)
	return s
}

// spread is a random extra wait of up to a fifth of d (at most 2 s), so
// that requests held together are not released together.
func spread(d time.Duration) time.Duration {
	s := min(d/5, 2*time.Second)
	if s <= 0 {
		return 0
	}
	return rand.N(s + 1)
}

// rateWait is how long to wait after the n-th (0-based) 429 of one call.
func rateWait(n int, retryAfter time.Duration) time.Duration {
	d := retryAfter
	if d <= 0 {
		d = rateWaitMin << min(n, 6)
	}
	return min(d, rateWaitMax)
}

// bucket is a token bucket: at most burst calls at once, then one every
// 1/rate. Callers queue in order of arrival (a reservation each), so a
// burst of workers is spread out rather than racing.
type bucket struct {
	mu     sync.Mutex
	every  time.Duration // one token per every
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(perMinute, burst int) *bucket {
	if perMinute <= 0 {
		return nil
	}
	return &bucket{every: time.Minute / time.Duration(perMinute), burst: float64(burst), tokens: float64(burst), last: time.Now()}
}

// take waits for a token. A nil bucket never waits.
func (b *bucket) take(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+float64(now.Sub(b.last))/float64(b.every))
	b.last = now
	b.tokens--
	var d time.Duration
	if b.tokens < 0 {
		d = time.Duration(-b.tokens * float64(b.every))
	}
	b.mu.Unlock()
	if d <= 0 {
		return nil
	}
	if err := sleepCtx(ctx, d); err != nil {
		b.mu.Lock()
		b.tokens++ // not used: give it back
		b.mu.Unlock()
		return err
	}
	return nil
}
