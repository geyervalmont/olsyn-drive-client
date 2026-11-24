//go:build !windows

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

type fakeHost struct {
	mu      sync.Mutex
	pushed  map[string][]string
	folder  string
	actions []string
	urls    []string
}

func (h *fakeHost) Push(window string, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pushed == nil {
		h.pushed = map[string][]string{}
	}
	h.pushed[window] = append(h.pushed[window], string(msg))
}

func (h *fakeHost) messages(window string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.pushed[window]...)
}

func (h *fakeHost) PickFolder(context.Context, string) (string, error) { return h.folder, nil }

func (h *fakeHost) WindowAction(window, action, route string) error {
	h.actions = append(h.actions, window+":"+action+":"+route)
	return nil
}

func (h *fakeHost) OpenURL(u string) error { h.urls = append(h.urls, u); return nil }

type seen struct {
	mu     sync.Mutex
	method string
	path   string
	query  string
	origin string
	body   string
}

func newBridge(t *testing.T, agent http.Handler) (*Bridge, *fakeHost) {
	t.Helper()
	dir, err := os.MkdirTemp("", "odbr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: agent}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := &fakeHost{}
	a := &agentconn.Agent{HTTP: agentconn.NewHTTPClient(agentconn.UnixDialer{Path: sock}), UserAgent: "test-ui"}
	b := New(ctx, a, host, nil)
	b.FlushEvery = 10 * time.Millisecond
	return b, host
}

func do(h http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "wails.localhost"
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProxyForwardsWithoutOrigin(t *testing.T) {
	var s seen
	b, _ := newBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.method, s.path, s.query, s.origin, s.body = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Origin"), string(body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	h := b.Middleware(http.NotFoundHandler())
	rec := do(h, http.MethodPatch, "/agent/v1/settings?x=1", `{"notify":true}`, map[string]string{"Origin": "http://wails.localhost"})
	if rec.Code != http.StatusAccepted || rec.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if s.method != "PATCH" || s.path != "/v1/settings" || s.query != "x=1" || s.origin != "" || s.body != `{"notify":true}` {
		t.Fatalf("agent saw %s %s ?%s origin=%q body=%q", s.method, s.path, s.query, s.origin, s.body)
	}
	// Other paths fall through to the page's files.
	if rec := do(h, http.MethodGet, "/index.html", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("asset fallthrough %d", rec.Code)
	}
}

func TestProxyRefusals(t *testing.T) {
	b, _ := newBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("agent reached: %s", r.URL.Path) }))
	cases := []struct {
		name, method, path, body string
		header                   map[string]string
		want                     int
	}{
		{"foreign origin", "GET", "/agent/v1/state", "", map[string]string{"Origin": "https://example.com"}, 403},
		{"not v1", "GET", "/agent/admin", "", nil, 404},
		{"dot dot", "GET", "/agent/v1/../secret", "", nil, 404},
		{"events direct", "GET", "/agent/v1/events", "", nil, 404},
		{"delete", "DELETE", "/agent/v1/state", "", nil, 405},
		{"too big", "POST", "/agent/v1/send", strings.Repeat("x", MaxRequestBody+1), nil, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := do(b, c.method, c.path, c.body, c.header); rec.Code != c.want {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestProxyAgentDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &agentconn.Agent{HTTP: agentconn.NewHTTPClient(agentconn.UnixDialer{Path: filepath.Join(os.TempDir(), "odbr-none.sock")}), UserAgent: "t"}
	b := New(ctx, a, &fakeHost{}, nil)
	rec := do(b, "GET", "/agent/v1/state", "", nil)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 503 || body["error"] != "agent_unreachable" {
		t.Fatalf("status %d body %v", rec.Code, body)
	}
}

func TestStreamPushesEventsToTheWindow(t *testing.T) {
	var gotLast string
	release := make(chan struct{})
	b, host := newBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			fmt.Fprint(w, `{"connection":"online"}`)
			return
		}
		gotLast = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 10\nevent: snapshot\ndata: {\"state\":{}}\n\n")
		w.(http.Flusher).Flush()
		for i := 11; i < 14; i++ {
			fmt.Fprintf(w, "id: %d\nevent: summary\ndata: {\"active\":%d}\n\n", i, i)
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	rec := do(b, "POST", "/bridge/stream", `{"window":"popover","last_event_id":"9"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("open stream %d", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		ids = ids[:0]
		for _, m := range host.messages("popover") {
			var msg struct {
				Kind   string
				Events []struct{ ID, Event string }
			}
			_ = json.Unmarshal([]byte(m), &msg)
			for _, e := range msg.Events {
				ids = append(ids, e.ID)
			}
		}
		if len(ids) == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Join(ids, ",") != "10,11,12,13" {
		t.Fatalf("event ids %v", ids)
	}
	if first := host.messages("popover")[0]; !strings.Contains(first, `"status":"open"`) {
		t.Fatalf("first message %s", first)
	}
	if gotLast != "9" {
		t.Fatalf("Last-Event-ID %q", gotLast)
	}
	if rec := do(b, "POST", "/bridge/stream/close", `{"window":"popover"}`, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("close %d", rec.Code)
	}
	close(release)
	if rec := do(b, "POST", "/bridge/stream", `{"window":"settings"}`, nil); rec.Code != 422 {
		t.Fatalf("unknown window %d", rec.Code)
	}
}

func TestBridgeCalls(t *testing.T) {
	b, host := newBridge(t, http.NotFoundHandler())
	host.folder = `C:\Users\Designer\Desktop`
	rec := do(b, "POST", "/bridge/pick-folder", `{"title":"Save a copy"}`, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Desktop") {
		t.Fatalf("pick-folder %d %s", rec.Code, rec.Body.String())
	}
	host.folder = ""
	if rec := do(b, "POST", "/bridge/pick-folder", `{}`, nil); !strings.Contains(rec.Body.String(), `"path":null`) {
		t.Fatalf("cancelled pick %s", rec.Body.String())
	}
	if rec := do(b, "POST", "/bridge/window", `{"window":"popover","action":"show-main","route":"transfers/held"}`, nil); rec.Code != 204 {
		t.Fatalf("window %d", rec.Code)
	}
	if rec := do(b, "POST", "/bridge/window", `{"window":"popover","action":"format-disk"}`, nil); rec.Code != 422 {
		t.Fatalf("bad action %d", rec.Code)
	}
	if rec := do(b, "POST", "/bridge/open-url", `{"url":"https://drive.olsyn.com/activate"}`, nil); rec.Code != 204 {
		t.Fatalf("open-url %d", rec.Code)
	}
	for _, bad := range []string{"file:///C:/Windows", "javascript:alert(1)", "http://example.com"} {
		if rec := do(b, "POST", "/bridge/open-url", `{"url":"`+bad+`"}`, nil); rec.Code != 422 {
			t.Fatalf("open-url %s allowed: %d", bad, rec.Code)
		}
	}
	if rec := do(b, "GET", "/bridge/window", "", nil); rec.Code != 405 {
		t.Fatalf("GET bridge %d", rec.Code)
	}
	if strings.Join(host.actions, "|") != "popover:show-main:transfers/held" || len(host.urls) != 1 {
		t.Fatalf("host saw %v %v", host.actions, host.urls)
	}
}
