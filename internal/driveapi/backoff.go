package driveapi

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff is an exponential backoff with full jitter, honouring the
// server's Retry-After when it is longer.
type Backoff struct {
	Base time.Duration // first wait, default 500 ms
	Max  time.Duration // cap, default 60 s
}

// Delay is the wait before retry attempt n (0-based) after err.
func (b Backoff) Delay(n int, err error) time.Duration {
	base, maxd := b.Base, b.Max
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if maxd <= 0 {
		maxd = 60 * time.Second
	}
	d := base
	for i := 0; i < n && d < maxd; i++ {
		d *= 2
	}
	if d > maxd {
		d = maxd
	}
	d = d/2 + rand.N(d/2+1)
	if ra := RetryAfter(err); ra > d {
		d = ra
	}
	return d
}

// Retry calls fn until it succeeds, fails with a non-retryable error, ctx
// ends, or attempts (when > 0) run out. It returns fn's last error.
func Retry(ctx context.Context, b Backoff, attempts int, fn func(ctx context.Context) error) error {
	for n := 0; ; n++ {
		err := fn(ctx)
		if err == nil || !IsRetryable(err) || (attempts > 0 && n+1 >= attempts) {
			return err
		}
		if serr := sleepCtx(ctx, b.Delay(n, err)); serr != nil {
			return err
		}
	}
}
