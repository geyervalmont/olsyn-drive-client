package vfs

import (
	"context"
	"time"
)

// Reading the changes feed at a pace the drive allows.
//
// The drive takes 60 changes calls a minute per device (429 with
// Retry-After 60 beyond), while its SSE stream wakes the client for every
// change, this PC's own uploads included: many a second during a bulk copy.
// So the changes loop (one call in flight at a time) reads the feed on a
// wake-up only once a minimum interval has passed since its last read; the
// wake-ups in between are covered by the read that follows them (there is
// always one after the last wake-up). The interval grows while this PC is
// itself sending many changes, which the feed would only echo back. Pages
// of one read (has_more) follow at once, but every call, pages included,
// counts against a budget a minute under the drive's limit, so normal use
// never meets a 429.

const (
	defaultChangesMinInterval = 2 * time.Second
	defaultChangesMaxInterval = 10 * time.Second
	defaultChangesPerMinute   = 40
	// heavyOwnRate is the rate of this PC's own changes reaching the drive
	// (a second) at which the interval is the longest.
	heavyOwnRate = 4.0
	// ownEchoWindow is how long a node this PC changed counts as "just
	// changed here": a stream event for it is this PC's own echo.
	ownEchoWindow = 15 * time.Second
)

// feedPacer is the changes loop's pace. It is used by that loop only.
type feedPacer struct {
	min, max  time.Duration
	perMinute int // <= 0: no budget
	calls     []time.Time
	lastEnd   time.Time
	lastOwn   uint64
	interval  time.Duration
}

func newFeedPacer(minI, maxI time.Duration, perMinute int) *feedPacer {
	return &feedPacer{min: minI, max: max(maxI, minI), perMinute: perMinute, interval: minI}
}

// take waits until one more changes call fits the budget, and counts it.
func (p *feedPacer) take(ctx context.Context) error {
	if p.perMinute <= 0 {
		return nil
	}
	for {
		now := time.Now()
		i := 0
		for i < len(p.calls) && now.Sub(p.calls[i]) >= time.Minute {
			i++
		}
		p.calls = append(p.calls[:0], p.calls[i:]...)
		if len(p.calls) < p.perMinute {
			p.calls = append(p.calls, now)
			return nil
		}
		d := p.calls[0].Add(time.Minute).Sub(now) + time.Millisecond
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// done records the end of a read; own is how many of this PC's changes
// had reached the drive by then. The next interval follows their rate.
func (p *feedPacer) done(now time.Time, own uint64) {
	if !p.lastEnd.IsZero() {
		rate := 0.0
		if dt := now.Sub(p.lastEnd).Seconds(); dt > 0 && own >= p.lastOwn {
			rate = float64(own-p.lastOwn) / dt
		}
		share := min(rate/heavyOwnRate, 1)
		p.interval = p.min + time.Duration(share*float64(p.max-p.min))
	}
	p.lastEnd, p.lastOwn = now, own
}

// wait is how long a wake-up at now waits before the next read.
func (p *feedPacer) wait(now time.Time) time.Duration {
	if p.lastEnd.IsZero() {
		return 0
	}
	return max(p.lastEnd.Add(p.interval).Sub(now), 0)
}
