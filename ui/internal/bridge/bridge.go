// Package bridge connects the webview to the agent. The webview cannot open a
// named pipe or a unix socket, so the shell serves two small HTTP surfaces on
// the webview's own origin (never on a network port):
//
//	/agent/v1/...   proxied as is to the agent (docs/ipc.md), minus the event stream
//	/bridge/...     the shell's own calls: open an event stream for a window,
//	                pick a folder, window actions, open a web page, and log a
//	                request that failed inside the page
//
// Events reach a window through Host.Push, which the Wails shell implements by
// calling window.__olsynBridge.receive(message) in that window.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

// Host is what the bridge needs from the native shell.
type Host interface {
	// Push delivers one JSON message to the named window's page.
	Push(window string, message []byte)
	// PickFolder asks for a folder; "" when the person cancels.
	PickFolder(ctx context.Context, title string) (string, error)
	// WindowAction runs show-main (optionally at a route), hide-popover or quit-ui.
	WindowAction(window, action, route string) error
	// OpenURL opens a web page in the default browser.
	OpenURL(url string) error
}

// MaxRequestBody is the agent's own limit (docs/ipc.md: 1 MiB per request).
const MaxRequestBody = 1 << 20

// maxResponseBody caps what is relayed back to the page.
const maxResponseBody = 16 << 20

// Bridge is an http.Handler for /agent/ and /bridge/.
type Bridge struct {
	agent *agentconn.Agent
	host  Host
	log   *slog.Logger

	// FlushEvery batches events per window so a busy copy is a few page calls a second, not hundreds.
	FlushEvery time.Duration

	mu      sync.Mutex
	streams map[string]*stream
	seq     uint64
	ctx     context.Context
}

type stream struct {
	cancel context.CancelFunc
	id     uint64
}

// New returns a bridge. ctx bounds every stream it opens.
func New(ctx context.Context, agent *agentconn.Agent, host Host, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{agent: agent, host: host, log: log, FlushEvery: 60 * time.Millisecond, streams: map[string]*stream{}, ctx: ctx}
}

// Middleware serves /agent/ and /bridge/ and passes everything else to next (the page's files).
func (b *Bridge) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/agent/"), strings.HasPrefix(r.URL.Path, "/bridge/"):
			b.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// ServeHTTP handles /agent/ and /bridge/ requests.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "forbidden_origin", "Requests from other pages are not accepted.")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/agent/") {
		b.proxy(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use POST.")
		return
	}
	switch r.URL.Path {
	case "/bridge/stream":
		b.openStream(w, r)
	case "/bridge/stream/close":
		b.closeStream(w, r)
	case "/bridge/pick-folder":
		b.pickFolder(w, r)
	case "/bridge/window":
		b.windowAction(w, r)
	case "/bridge/open-url":
		b.openURL(w, r)
	case "/bridge/log":
		b.pageLog(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "Unknown bridge call.")
	}
}

// sameOrigin accepts requests with no Origin or with the page's own origin, so
// nothing loaded from elsewhere can reach the agent through the shell.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

var allowedMethods = map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPatch: true}

func (b *Bridge) proxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/agent")
	if !strings.HasPrefix(path, "/v1/") || strings.Contains(path, "..") {
		writeError(w, http.StatusNotFound, "not_found", "Unknown agent path.")
		return
	}
	if path == "/v1/events" {
		writeError(w, http.StatusNotFound, "not_found", "Events come through /bridge/stream.")
		return
	}
	if !allowedMethods[r.Method] {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed.")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Could not read the request.")
		return
	}
	if len(body) > MaxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "bad_request", "The request is too large.")
		return
	}
	target := agentconn.BaseURL + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	resp, err := b.forward(r.Context(), r.Method, target, body)
	var ensureErr error
	if err != nil && agentconn.NotRunning(err) {
		// Nothing reached the agent, so after starting it the request can be
		// sent again, POSTs included: the page should not see the agent's
		// first seconds as a failure.
		b.log.Warn("agent request failed; starting the agent and trying again", "method", r.Method, "path", path, "err", err)
		ensureCtx, cancel := context.WithTimeout(r.Context(), agentconn.StartWait+time.Second)
		ensureErr = b.agent.EnsureRunning(ensureCtx)
		cancel()
		if ensureErr == nil {
			resp, err = b.forward(r.Context(), r.Method, target, body)
		}
	}
	if err != nil {
		b.log.Warn("agent request failed", "method", r.Method, "path", path, "err", err)
		var be *blocked.Error
		if errors.As(ensureErr, &be) {
			// Security software removed or blocked the agent: starting it
			// again would not help; say what happened.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent_blocked", "message": be.Error(),
				"title": be.Title(), "detail": be.Detail(), "help_url": blocked.HelpURL})
			return
		}
		go b.ensureAgent()
		writeError(w, http.StatusServiceUnavailable, "agent_unreachable", "Olsyn Drive isn't answering on this PC. Starting it again.")
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		b.log.Warn("agent request failed while reading the answer", "method", r.Method, "path", path, "status", resp.StatusCode, "err", err)
		writeError(w, http.StatusBadGateway, "agent_unreachable", "Olsyn Drive stopped answering. Try again.")
		return
	}
	if resp.StatusCode >= 400 {
		// Only the error code is logged: messages can name files.
		var e struct {
			Code string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		b.log.Warn("agent refused a request", "method", r.Method, "path", path, "status", resp.StatusCode, "code", truncate(e.Code, 64))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(data); err != nil {
		b.log.Warn("relay agent response", "method", r.Method, "path", path, "err", err)
	}
}

// forward sends one request to the agent: a fresh request with no Origin, no
// cookies, only what the agent needs.
func (b *Bridge) forward(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("build agent request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", b.agent.UserAgent)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.agent.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// pageLog records a request that failed inside the page (so it never reached
// the shell): method, path and the browser's error, never a body.
func (b *Bridge) pageLog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Error  string `json:"error"`
	}
	if !decode(w, r, &in) {
		return
	}
	path, _, _ := strings.Cut(in.Path, "?")
	b.log.Warn("page request failed", "method", truncate(in.Method, 10), "path", truncate(path, 200), "err", truncate(in.Error, 300))
	w.WriteHeader(http.StatusNoContent)
}

func (b *Bridge) ensureAgent() {
	ctx, cancel := context.WithTimeout(b.ctx, agentconn.StartWait+5*time.Second)
	defer cancel()
	if err := b.agent.EnsureRunning(ctx); err != nil {
		b.log.Warn("agent not running", "err", err)
	}
}

// message is what Host.Push delivers (the front end's BridgeMessage).
type message struct {
	Kind    string     `json:"kind"`
	Status  string     `json:"status,omitempty"`
	Blocked *blockInfo `json:"blocked,omitempty"`
	Message string     `json:"message,omitempty"`
	Events  []sseEvent `json:"events,omitempty"`
	Paths   []string   `json:"paths,omitempty"`
	Route   string     `json:"route,omitempty"`
}

// blockInfo tells the page that security software removed or blocked the
// agent, in words for a screen.
type blockInfo struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	HelpURL string `json:"help_url"`
}

type sseEvent struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	Data  string `json:"data"`
}

// Send pushes a message to a window; used for drops and routes too.
func (b *Bridge) send(window string, m message) {
	data, err := json.Marshal(m)
	if err != nil {
		b.log.Error("encode bridge message", "kind", m.Kind, "err", err)
		return
	}
	b.host.Push(window, data)
}

// SendDrop tells a window that paths were dropped on it.
func (b *Bridge) SendDrop(window string, paths []string) {
	b.send(window, message{Kind: "drop", Paths: paths})
}

// SendRoute asks a window to show a route such as "transfers/held".
func (b *Bridge) SendRoute(window, route string) {
	b.send(window, message{Kind: "route", Route: route})
}

func (b *Bridge) openStream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Window      string `json:"window"`
		LastEventID string `json:"last_event_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !validWindow(in.Window) {
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Unknown window.")
		return
	}
	ctx, cancel := context.WithCancel(b.ctx)
	b.mu.Lock()
	if old := b.streams[in.Window]; old != nil {
		old.cancel()
	}
	b.seq++
	s := &stream{cancel: cancel, id: b.seq}
	b.streams[in.Window] = s
	b.mu.Unlock()

	go b.runStream(ctx, s, in.Window, in.LastEventID)
	w.WriteHeader(http.StatusAccepted)
}

func (b *Bridge) runStream(ctx context.Context, s *stream, window, lastID string) {
	defer func() {
		b.mu.Lock()
		if b.streams[window] == s {
			delete(b.streams, window)
		}
		b.mu.Unlock()
		s.cancel()
	}()

	var (
		mu      sync.Mutex
		pending []sseEvent
	)
	flush := func() {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		if len(batch) > 0 && ctx.Err() == nil {
			b.send(window, message{Kind: "events", Events: batch})
		}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(b.FlushEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				flush()
			}
		}
	}()

	err := agentconn.Stream(ctx, b.agent.HTTP, b.agent.UserAgent, lastID, func() {
		b.send(window, message{Kind: "stream", Status: "open"})
	}, func(e agentconn.Event) error {
		mu.Lock()
		pending = append(pending, sseEvent{ID: e.ID, Event: e.Name, Data: e.Data})
		n := len(pending)
		mu.Unlock()
		// A snapshot goes at once so a window paints without waiting for the tick.
		if e.Name == "snapshot" || n >= 256 {
			flush()
		}
		return nil
	})
	close(done)
	flush()
	if ctx.Err() != nil {
		return // closed by the page or replaced by a newer stream
	}
	b.log.Info("window event stream ended", "window", window, "err", err)
	// Start the agent if it is gone, then let the page reconnect on its own backoff.
	ensureCtx, cancel := context.WithTimeout(b.ctx, agentconn.StartWait+5*time.Second)
	ensureErr := b.agent.EnsureRunning(ensureCtx)
	cancel()
	msg := "The connection to Olsyn Drive was lost."
	if ensureErr != nil {
		msg = "Olsyn Drive isn't running on this PC."
	}
	m := message{Kind: "stream", Status: "error", Message: msg}
	var be *blocked.Error
	if errors.As(ensureErr, &be) {
		m.Message = be.Error()
		m.Blocked = &blockInfo{Kind: string(be.Kind), Title: be.Title(), Detail: be.Detail(), HelpURL: blocked.HelpURL}
	}
	b.send(window, m)
}

func (b *Bridge) closeStream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Window string `json:"window"`
	}
	if !decode(w, r, &in) {
		return
	}
	b.mu.Lock()
	if s := b.streams[in.Window]; s != nil {
		s.cancel()
		delete(b.streams, in.Window)
	}
	b.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// CloseAll ends every window stream (on quit).
func (b *Bridge) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, s := range b.streams {
		s.cancel()
		delete(b.streams, name)
	}
}

func (b *Bridge) pickFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &in) {
		return
	}
	path, err := b.host.PickFolder(r.Context(), truncate(in.Title, 200))
	if err != nil {
		b.log.Warn("folder picker failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "The folder picker could not open.")
		return
	}
	out := struct {
		Path *string `json:"path"`
	}{}
	if path != "" {
		out.Path = &path
	}
	writeJSON(w, http.StatusOK, out)
}

func (b *Bridge) windowAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Window string `json:"window"`
		Action string `json:"action"`
		Route  string `json:"route"`
	}
	if !decode(w, r, &in) {
		return
	}
	switch in.Action {
	case "show-main", "hide-popover", "quit-ui":
	default:
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Unknown window action.")
		return
	}
	if err := b.host.WindowAction(in.Window, in.Action, truncate(in.Route, 200)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "The window action failed.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *Bridge) openURL(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid", "Only https links can be opened.")
		return
	}
	if err := b.host.OpenURL(u.String()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "The browser could not be opened.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validWindow(name string) bool { return name == "main" || name == "popover" }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "The request was not valid JSON.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrNoWindow is returned by hosts for an unknown window name.
var ErrNoWindow = errors.New("no such window")

// Describe is a log-safe summary of a bridge message (kind and size only).
func Describe(msg []byte) string {
	var m struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(msg, &m)
	return fmt.Sprintf("%s (%d bytes)", m.Kind, len(msg))
}
