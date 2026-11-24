package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client talks to the agent over its local endpoint (used by the CLI; the UI
// does the same in its own language).
type Client struct {
	http *http.Client
}

// NewClient returns a client that reaches the agent through dial.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: 0,
	}
	return &Client{http: &http.Client{Transport: tr}}
}

// APIError is an error answer from the agent.
type APIError struct {
	Status  int
	Code    string
	Message string
	Body    map[string]any
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("agent answered %d %s", e.Status, e.Code)
}

// Do sends a request; body (if not nil) is JSON-encoded and the answer is
// decoded into out (if not nil).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://olsyn-drive"+path, rd)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the agent: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read agent answer: %w", err)
	}
	if resp.StatusCode >= 400 {
		e := &APIError{Status: resp.StatusCode}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			e.Body = m
			e.Code, _ = m["error"].(string)
			e.Message, _ = m["message"].(string)
		}
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode agent answer: %w", err)
		}
	}
	return nil
}

// State returns GET /v1/state.
func (c *Client) State(ctx context.Context) (State, error) {
	var s State
	err := c.Do(ctx, http.MethodGet, "/v1/state", nil, &s)
	return s, err
}

// StreamEvent is one received server-sent event.
type StreamEvent struct {
	ID   string
	Type string
	Data json.RawMessage
}

// Events reads GET /v1/events until ctx ends, the stream closes, or fn
// returns an error (errStop ends it quietly).
func (c *Client) Events(ctx context.Context, lastEventID string, fn func(StreamEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://olsyn-drive/v1/events", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var ev StreamEvent
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.Type != "" || data.Len() > 0 {
				ev.Data = json.RawMessage(data.String())
				if err := fn(ev); err != nil {
					if errors.Is(err, ErrStopStream) {
						return nil
					}
					return err
				}
			}
			ev, data = StreamEvent{}, strings.Builder{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id: "):
			ev.ID = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.Type = line[7:]
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(line[6:])
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}

// ErrStopStream ends Events without an error.
var ErrStopStream = errors.New("stop stream")
