package agentconn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Event is one Server-Sent Event from GET /v1/events. Data is one line of JSON.
type Event struct {
	ID   string
	Name string
	Data string
}

// SilenceTimeout: the agent pings every 15 s; nothing for this long means the stream is dead.
const SilenceTimeout = 45 * time.Second

// ErrStreamEnded is returned when the agent closes the stream.
var ErrStreamEnded = errors.New("agent event stream ended")

// ReadEvents parses an SSE stream, calling fn for each event, until r ends or
// fn returns an error. onActivity (optional) runs for every line, pings included.
func ReadEvents(r io.Reader, onActivity func(), fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024) // a snapshot with 50 transfers is well under this
	var (
		id, name string
		data     []string
		hasData  bool
	)
	for sc.Scan() {
		if onActivity != nil {
			onActivity()
		}
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if hasData {
				if err := fn(Event{ID: id, Name: orDefault(name, "message"), Data: strings.Join(data, "\n")}); err != nil {
					return err
				}
			}
			name, data, hasData = "", nil, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment, e.g. ": ping"
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data = append(data, value)
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				id = value
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read agent events: %w", err)
	}
	return ErrStreamEnded
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Stream opens GET /v1/events (resuming after lastID when given) and calls fn
// for each event until ctx ends or the stream fails. onOpen runs once the agent
// has accepted the stream.
func Stream(ctx context.Context, client *http.Client, userAgent, lastID string, onOpen func(), fn func(Event) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+"/v1/events", nil)
	if err != nil {
		return fmt.Errorf("build events request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("open agent events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("open agent events: status %d", resp.StatusCode)
	}
	if onOpen != nil {
		onOpen()
	}
	// Watchdog: cancel the request if the agent goes quiet.
	activity := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(SilenceTimeout)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activity:
				if !t.Stop() {
					<-t.C
				}
				t.Reset(SilenceTimeout)
			case <-t.C:
				cancel()
				return
			}
		}
	}()
	err = ReadEvents(resp.Body, func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}, fn)
	if ctx.Err() != nil && !errors.Is(err, ErrStreamEnded) {
		return fmt.Errorf("agent events went quiet or were closed: %w", ctx.Err())
	}
	return err
}
