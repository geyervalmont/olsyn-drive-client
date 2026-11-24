package core

import (
	"slices"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// progress tracks running uploads: bytes, rate, start.
type progress struct {
	mu    sync.Mutex
	items map[uint64]*prog
	// rate over all transfers: bytes counted since the last sample
	sampleAt    time.Time
	sampleBytes int64
	counted     int64
	bps         int64
}

type prog struct {
	bytes, total, bps int64
	started, at       time.Time
	note              string // the engine's note on the finished upload
}

func newProgress() *progress { return &progress{items: map[uint64]*prog{}, sampleAt: time.Now()} }

func (p *progress) note(seq uint64, e *transfer.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.items[seq]
	now := time.Now()
	if it == nil {
		it = &prog{started: now.UTC(), at: now}
		p.items[seq] = it
	}
	if d := e.Bytes - it.bytes; d > 0 {
		p.counted += d
		if dt := now.Sub(it.at).Seconds(); dt > 0.05 {
			rate := int64(float64(d) / dt)
			it.bps = (it.bps + rate) / 2
			it.at = now
		}
	}
	if e.Kind == transfer.EventStarted {
		it.at = now
	}
	it.bytes, it.total = max(e.Bytes, 0), max(e.Total, it.total)
	if e.Note != "" {
		it.note = e.Note
	}
}

func (p *progress) get(seq uint64) *prog {
	p.mu.Lock()
	defer p.mu.Unlock()
	if it := p.items[seq]; it != nil {
		c := *it
		return &c
	}
	return nil
}

// done forgets a finished transfer and returns what was known of it.
func (p *progress) done(seq uint64) *prog {
	p.mu.Lock()
	defer p.mu.Unlock()
	it := p.items[seq]
	delete(p.items, seq)
	return it
}

// totals returns bytes done of running transfers and the overall rate.
func (p *progress) totals() (done, bps int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.items {
		done += it.bytes
	}
	if dt := time.Since(p.sampleAt).Seconds(); dt >= 1 {
		rate := int64(float64(p.counted-p.sampleBytes) / dt)
		p.bps = (p.bps + rate) / 2
		p.sampleAt, p.sampleBytes = time.Now(), p.counted
	}
	return done, p.bps
}

// history keeps finished transfers: the last max, no older than keep.
type history struct {
	mu    sync.Mutex
	max   int
	keep  time.Duration
	items []agent.Transfer // oldest first
}

func newHistory(max int, keep time.Duration) *history { return &history{max: max, keep: keep} }

func (h *history) add(t agent.Transfer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, t)
	h.trimLocked()
}

func (h *history) trimLocked() {
	if over := len(h.items) - h.max; over > 0 {
		h.items = slices.Delete(h.items, 0, over)
	}
	cut := time.Now().Add(-h.keep)
	i := 0
	for i < len(h.items) && h.items[i].FinishedAt != nil && h.items[i].FinishedAt.Before(cut) {
		i++
	}
	h.items = slices.Delete(h.items, 0, i)
}

// list returns finished transfers, newest first.
func (h *history) list() []agent.Transfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trimLocked()
	out := make([]agent.Transfer, len(h.items))
	for i, t := range h.items {
		out[len(h.items)-1-i] = t
	}
	return out
}

func (h *history) get(id string) (agent.Transfer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.items {
		if t.ID == id {
			return t, true
		}
	}
	return agent.Transfer{}, false
}

func (h *history) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = slices.DeleteFunc(h.items, func(t agent.Transfer) bool { return t.ID == id })
}

func (h *history) clear() {
	h.mu.Lock()
	h.items = nil
	h.mu.Unlock()
}

func (h *history) replace(items []agent.Transfer) {
	h.mu.Lock()
	h.items = items
	h.trimLocked()
	h.mu.Unlock()
}

func (h *history) snapshot() []agent.Transfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.items)
}
