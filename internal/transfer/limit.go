package transfer

import (
	"context"
	"sync"
	"time"
)

// gate holds transfers while paused.
type gate struct {
	mu     sync.Mutex
	paused bool
	open   chan struct{} // closed while running
}

func newGate() *gate {
	g := &gate{open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *gate) pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		g.paused = true
		g.open = make(chan struct{})
	}
}

func (g *gate) resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		g.paused = false
		close(g.open)
	}
}

func (g *gate) isPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// limiter is a token bucket in bytes per second (0 = unlimited).
type limiter struct {
	mu     sync.Mutex
	rate   int64
	tokens float64
	last   time.Time
}

func (l *limiter) set(rate int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate = rate
	l.tokens = 0
	l.last = time.Now()
}

func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
	if burst := float64(l.rate); l.tokens > burst {
		l.tokens = burst
	}
	l.last = now
	l.tokens -= float64(n)
	var d time.Duration
	if l.tokens < 0 {
		d = time.Duration(-l.tokens / float64(l.rate) * float64(time.Second))
	}
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttled is a reader that honours the gate and limiter and counts bytes.
type throttled struct {
	ctx   context.Context
	r     interface{ Read([]byte) (int, error) }
	g     *gate
	l     *limiter
	count func(int)
}

const chunk = 64 << 10

func (t *throttled) Read(p []byte) (int, error) {
	if err := t.g.wait(t.ctx); err != nil {
		return 0, err
	}
	if len(p) > chunk {
		p = p[:chunk]
	}
	if err := t.l.wait(t.ctx, len(p)); err != nil {
		return 0, err
	}
	n, err := t.r.Read(p)
	if n > 0 && t.count != nil {
		t.count(n)
	}
	return n, err
}
