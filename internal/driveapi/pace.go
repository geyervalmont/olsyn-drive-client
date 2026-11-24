package driveapi

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Waiting for the drive when it asks (429 rate_limited with Retry-After).
//
// A 429 is "wait", never a failure: send waits as the drive asked and tries
// again, a bounded number of times, before it hands the 429 to the caller
// (which classifies it as retryable). The wait is shared: one 429 holds every
// request this Client makes to the drive until the time is up, so parallel
// workers do not each hit the limit in turn, and each waiter adds its own
// small random delay so they do not all come back at the same instant.

const (
	// rateRetries is how many 429 answers one call waits out before it
	// returns the 429 to its caller.
	rateRetries = 8
	// rateWaitMax caps one wait, whatever Retry-After says.
	rateWaitMax = 2 * time.Minute
	// rateWaitMin is the wait when a 429 comes without Retry-After (doubled
	// on each further 429 of the same call).
	rateWaitMin = time.Second
)

// pacer holds requests while the drive has asked to wait.
type pacer struct {
	mu    sync.Mutex
	until time.Time
}

// hold makes every request wait until d from now (or longer, if an earlier
// 429 asked for more).
func (p *pacer) hold(d time.Duration) {
	t := time.Now().Add(d)
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.After(p.until) {
		p.until = t
	}
}

// wait returns once the drive's wait is over (plus this caller's jitter), or
// when ctx ends.
func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	d := time.Until(p.until)
	p.mu.Unlock()
	if d <= 0 {
		return nil
	}
	return sleepCtx(ctx, d+spread(d))
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
