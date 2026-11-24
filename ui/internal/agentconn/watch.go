package agentconn

import (
	"context"
	"encoding/json"
	"time"
)

// WatchHandlers receive what the shell's own subscription sees.
type WatchHandlers struct {
	// State runs with the latest state whenever it, or the summary, changes.
	State func(State)
	// Notification runs for each notification event.
	Notification func(Notification)
	// Down runs when the stream is lost and the agent does not answer.
	Down func(err error)
}

// Watch keeps one event stream open for the shell (tray icon, tooltip and
// toasts), reconnecting with backoff 0.25 s doubling to 5 s for as long as
// ctx lasts and starting the agent when it is gone. It returns when ctx ends.
func (a *Agent) Watch(ctx context.Context, h WatchHandlers) {
	var (
		last    State
		have    bool
		lastID  string
		backoff = 250 * time.Millisecond
		lost    bool
	)
	for ctx.Err() == nil {
		err := Stream(ctx, a.HTTP, a.UserAgent, lastID, func() {
			backoff = 250 * time.Millisecond
			if lost {
				a.info("agent event stream open again")
				lost = false
			}
		}, func(e Event) error {
			if e.ID != "" {
				lastID = e.ID
			}
			switch e.Name {
			case "snapshot":
				var s Snapshot
				if json.Unmarshal([]byte(e.Data), &s) == nil {
					last, have = s.State, true
					h.emitState(last)
				}
			case "state":
				var s State
				if json.Unmarshal([]byte(e.Data), &s) == nil {
					last, have = s, true
					h.emitState(last)
				}
			case "summary":
				if have && json.Unmarshal([]byte(e.Data), &last.Transfers) == nil {
					h.emitState(last)
				}
			case "notification":
				var n Notification
				if json.Unmarshal([]byte(e.Data), &n) == nil && h.Notification != nil {
					h.Notification(n)
				}
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if !lost {
			// Logged once per loss, not once per retry.
			a.warn("agent event stream lost", "err", err)
			lost = true
		}
		if ensureErr := a.EnsureRunning(ctx); ensureErr != nil && h.Down != nil {
			h.Down(ensureErr)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (h WatchHandlers) emitState(s State) {
	if h.State != nil {
		h.State(s)
	}
}
