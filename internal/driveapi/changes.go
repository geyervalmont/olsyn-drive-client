package driveapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Change ops in the changes feed.
const (
	OpUpsert  = "upsert"
	OpTrashed = "trashed"
	OpRemoved = "removed"
	OpResync  = "resync"
)

// Change is one entry of the changes feed.
type Change struct {
	Seq      int64  `json:"seq"`
	Op       string `json:"op"`
	Node     *Node  `json:"node,omitempty"`
	ID       string `json:"id,omitempty"`        // removed
	FolderID string `json:"folder_id,omitempty"` // resync
	// Resync is "subtree" on an upsert of a folder that came into view:
	// re-list its whole subtree.
	Resync string `json:"resync,omitempty"`
}

// Reset tells the client to drop its index (or the folder scope) and
// re-list, then continue from Next.
type Reset struct {
	Reason string `json:"reason"`
}

// Reset reasons.
const (
	ResetNoCursor      = "no_cursor"
	ResetCursorInvalid = "cursor_invalid"
	ResetCursorExpired = "cursor_expired"
	ResetAccessChanged = "access_changed"
	ResetScopeLost     = "scope_lost"
)

// ChangesPage is one page of the changes feed.
type ChangesPage struct {
	Changes []Change `json:"changes"`
	Next    int64    `json:"next"`
	HasMore bool     `json:"has_more"`
	Reset   *Reset   `json:"reset"`
	// RetryAfter is set when the server is holding the feed at a horizon:
	// poll again after it.
	RetryAfter time.Duration `json:"-"`
}

// ChangesMaxPage is the feed's largest page.
const ChangesMaxPage = 500

// Changes reads the feed after cursor since. since < 0 means "no cursor":
// the answer is a reset with the current position, to take BEFORE a full
// listing. folder limits the feed to one subtree ("" for everything).
func (c *Client) Changes(ctx context.Context, since int64, limit int, folder string) (*ChangesPage, error) {
	q := url.Values{}
	if since >= 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if limit > 0 {
		if limit > ChangesMaxPage {
			limit = ChangesMaxPage
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	if folder != "" {
		q.Set("folder", folder)
	}
	resp, err := c.send(ctx, request{op: "changes", method: http.MethodGet, path: "/api/v1/changes", query: q})
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	var out ChangesPage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, &Error{Op: "changes", Err: err}
	}
	out.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	return &out, nil
}

// StreamEvent is one Server-Sent Event from /api/v1/stream.
type StreamEvent struct {
	Event string // "change", "notification", "transform.run", …
	ID    string
	Data  json.RawMessage
}

// StreamChange is the data of a "change" event.
type StreamChange struct {
	Type     string `json:"type"`
	NodeID   string `json:"node_id"`
	ParentID string `json:"parent_id"`
	Seq      int64  `json:"seq"`
}

// Stream opens the SSE stream and calls fn for each event until the stream
// ends, ctx is cancelled or fn returns an error. onOpen (may be nil) is
// called once the stream is connected. The stream is NOT resumable: after
// any reconnect, read the changes feed. A clean end returns nil.
func (c *Client) Stream(ctx context.Context, onOpen func(), fn func(StreamEvent) error) error {
	resp, err := c.send(ctx, request{op: "stream", method: http.MethodGet, path: "/api/v1/stream",
		header: http.Header{"Accept": {"text/event-stream"}}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if onOpen != nil {
		onOpen()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var ev StreamEvent
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if data.Len() > 0 || ev.Event != "" {
				if ev.Event == "" {
					ev.Event = "message"
				}
				ev.Data = json.RawMessage(data.String())
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = StreamEvent{}
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / ping
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "id":
			ev.ID = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Op: "stream", Err: err}
	}
	return nil
}
