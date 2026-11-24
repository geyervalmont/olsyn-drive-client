package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event hub limits (docs/ipc.md, Events).
const (
	ringSize        = 1024
	subscriberQueue = 256
	maxSubscribers  = 16
	coalesceEvery   = 250 * time.Millisecond
)

// ErrTooManySubscribers is returned when maxSubscribers streams are open.
var ErrTooManySubscribers = errors.New("too many event streams")

// Event is one server-sent event.
type Event struct {
	Seq      uint64
	ID       string
	Type     string
	Key      string // transfer id, "summary", ...; with Coalesce, later events of the same Type+Key replace it
	Coalesce bool
	Data     json.RawMessage
}

type eventKey struct{ typ, key string }

// Hub fans events out to subscribers. Publishers never block: progress-like
// events are throttled per key and replaced in slow queues, and a queue that
// overflows is dropped in favour of a fresh snapshot.
type Hub struct {
	run string

	mu      sync.Mutex
	seq     uint64
	ring    []Event // last ringSize events, oldest first
	subs    map[*Subscriber]struct{}
	pending map[eventKey]json.RawMessage
	order   []eventKey // pending keys in arrival order
}

// NewHub returns a hub whose event ids start with run.
func NewHub(run string) *Hub {
	return &Hub{run: run, subs: map[*Subscriber]struct{}{}, pending: map[eventKey]json.RawMessage{}}
}

// Run flushes throttled events every 250 ms until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(coalesceEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Flush()
		}
	}
}

// Publish sends an event now. It is never merged away.
func (h *Hub) Publish(typ string, v any) {
	h.publish(typ, "", false, v)
}

// PublishNow sends a keyed event now (a transfer's state change) and drops any
// throttled value of the same key, which it supersedes.
func (h *Hub) PublishNow(typ, key string, v any) {
	h.mu.Lock()
	k := eventKey{typ, key}
	if _, ok := h.pending[k]; ok {
		delete(h.pending, k)
		h.order = removeKey(h.order, k)
	}
	h.mu.Unlock()
	h.publish(typ, key, false, v)
}

// Throttle records the latest value for typ+key; Flush sends it, so each key
// produces at most one event per 250 ms.
func (h *Hub) Throttle(typ, key string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := eventKey{typ, key}
	if _, ok := h.pending[k]; !ok {
		h.order = append(h.order, k)
	}
	h.pending[k] = data
}

// Flush publishes every throttled value.
func (h *Hub) Flush() {
	h.mu.Lock()
	order, pending := h.order, h.pending
	h.order, h.pending = nil, map[eventKey]json.RawMessage{}
	for _, k := range order {
		h.appendLocked(k.typ, k.key, true, pending[k])
	}
	h.mu.Unlock()
}

func (h *Hub) publish(typ, key string, coalesce bool, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.appendLocked(typ, key, coalesce, data)
	h.mu.Unlock()
}

func (h *Hub) appendLocked(typ, key string, coalesce bool, data json.RawMessage) {
	h.seq++
	ev := Event{Seq: h.seq, ID: h.run + "-" + strconv.FormatUint(h.seq, 10), Type: typ, Key: key, Coalesce: coalesce, Data: data}
	if len(h.ring) == ringSize {
		copy(h.ring, h.ring[1:])
		h.ring = h.ring[:ringSize-1]
	}
	h.ring = append(h.ring, ev)
	for s := range h.subs {
		s.push(ev)
	}
}

// CurrentID is the id of the newest event (used as a snapshot's id).
func (h *Hub) CurrentID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run + "-" + strconv.FormatUint(h.seq, 10)
}

// Subscribe opens a stream. With a lastEventID from this run that is still in
// the ring, the events after it are queued; otherwise the subscriber starts
// by needing a snapshot.
func (h *Hub) Subscribe(lastEventID string) (*Subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxSubscribers {
		return nil, ErrTooManySubscribers
	}
	s := &Subscriber{notify: make(chan struct{}, 1), needSnapshot: true}
	if seq, ok := h.parseID(lastEventID); ok && len(h.ring) > 0 && seq >= h.ring[0].Seq-1 && seq <= h.seq {
		s.needSnapshot = false
		for _, ev := range h.ring {
			if ev.Seq > seq {
				s.queue = append(s.queue, ev)
			}
		}
	}
	h.subs[s] = struct{}{}
	s.signal()
	return s, nil
}

// Unsubscribe closes a stream.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Subscribers returns the number of open streams.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) parseID(id string) (uint64, bool) {
	run, seq, ok := strings.Cut(id, "-")
	if !ok || run != h.run {
		return 0, false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	return n, err == nil
}

// Subscriber is one event stream's queue.
type Subscriber struct {
	mu           sync.Mutex
	queue        []Event
	needSnapshot bool
	lagged       bool
	notify       chan struct{}
}

func (s *Subscriber) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Subscriber) push(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.needSnapshot && s.lagged {
		return // everything will be in the snapshot
	}
	if ev.Coalesce {
		// Replace a queued value of the same key, unless a non-mergeable event
		// of that key came after it (order must hold across state changes).
		for i := len(s.queue) - 1; i >= 0; i-- {
			q := s.queue[i]
			if q.Type != ev.Type || q.Key != ev.Key {
				continue
			}
			if q.Coalesce {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
			}
			break
		}
	}
	if len(s.queue) >= subscriberQueue {
		s.queue = nil
		s.needSnapshot = true
		s.lagged = true
		s.signal()
		return
	}
	s.queue = append(s.queue, ev)
	s.signal()
}

// Next waits for work. It returns needSnapshot (with lagged when the queue
// overflowed) and the queued events, in order.
func (s *Subscriber) Next(ctx context.Context, timeout time.Duration) (needSnapshot, lagged bool, events []Event, err error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		s.mu.Lock()
		if s.needSnapshot || len(s.queue) > 0 {
			needSnapshot, lagged, events = s.needSnapshot, s.lagged, s.queue
			s.needSnapshot, s.lagged, s.queue = false, false, nil
			s.mu.Unlock()
			return needSnapshot, lagged, events, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, false, nil, ctx.Err()
		case <-t.C:
			return false, false, nil, nil
		case <-s.notify:
		}
	}
}

func removeKey(keys []eventKey, k eventKey) []eventKey {
	for i, x := range keys {
		if x == k {
			return append(keys[:i], keys[i+1:]...)
		}
	}
	return keys
}

// formatSSE writes one event in text/event-stream form.
func formatSSE(id, typ string, data []byte) string {
	return fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", id, typ, data)
}
