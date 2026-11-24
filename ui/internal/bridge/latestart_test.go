//go:build !windows

package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

// lateAgent is an agent that is not running until Start is called, and then
// only starts listening after a delay, the way a freshly launched
// olsyn-drive agent takes a moment to create its endpoint.
type lateAgent struct {
	t       *testing.T
	sock    string
	delay   time.Duration
	handler http.Handler
	starts  atomic.Int32
	once    sync.Once
}

func (l *lateAgent) Start() error {
	l.starts.Add(1)
	l.once.Do(func() {
		go func() {
			time.Sleep(l.delay)
			ln, err := net.Listen("unix", l.sock)
			if err != nil {
				l.t.Errorf("listen: %v", err)
				return
			}
			srv := &http.Server{Handler: l.handler}
			l.t.Cleanup(func() { srv.Close() })
			go srv.Serve(ln)
		}()
	})
	return nil
}

func (l *lateAgent) String() string { return "test agent" }

func newLateAgent(t *testing.T, delay time.Duration, handler http.Handler) (*lateAgent, *agentconn.Agent, *bytes.Buffer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "odls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	la := &lateAgent{t: t, sock: filepath.Join(dir, "a.sock"), delay: delay, handler: handler}
	var logs bytes.Buffer
	a := &agentconn.Agent{
		HTTP:      agentconn.NewHTTPClient(agentconn.UnixDialer{Path: la.sock}),
		UserAgent: "test-ui",
		Starter:   la,
		Log:       slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil)),
	}
	return la, a, &logs
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// The first Windows run: the page asked the agent for something before the
// agent had created its endpoint. The request must reach the agent once it is
// up, not fail, and the failure must be logged.
func TestProxyAgentStartsLate(t *testing.T) {
	for _, delay := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			var signins atomic.Int32
			la, a, logs := newLateAgent(t, delay, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/state":
					fmt.Fprint(w, `{"connection":"signed_out"}`)
				case "/v1/signin":
					body, _ := io.ReadAll(r.Body)
					if string(body) != `{"open_browser":true}` {
						t.Errorf("signin body %q", body)
					}
					signins.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, `{"state":"waiting","user_code":"ABCD-EFGH"}`)
				}
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := New(ctx, a, &fakeHost{}, a.Log)

			start := time.Now()
			rec := do(b, http.MethodPost, "/agent/v1/signin", `{"open_browser":true}`, nil)
			if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "ABCD-EFGH") {
				t.Fatalf("signin through the bridge: %d %s (after %s)", rec.Code, rec.Body, time.Since(start))
			}
			if n := signins.Load(); n != 1 {
				t.Fatalf("the agent saw %d sign-ins, want 1", n)
			}
			if n := la.starts.Load(); n != 1 {
				t.Fatalf("started the agent %d times, want 1", n)
			}
			out := logs.String()
			for _, want := range []string{"agent request failed; starting the agent", "method=POST", "path=/v1/signin", "the started agent is answering"} {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "open_browser") {
				t.Errorf("a request body reached the log:\n%s", out)
			}
		})
	}
}

func TestProxyLogsAgentRefusals(t *testing.T) {
	_, a, logs := newLateAgent(t, 0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/state" {
			fmt.Fprint(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":"linking","message":"Secret Plans.pdf"}`)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.EnsureRunning(ctx); err != nil {
		t.Fatal(err)
	}
	b := New(ctx, a, &fakeHost{}, a.Log)
	rec := do(b, http.MethodPost, "/agent/v1/signin", `{}`, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "linking") {
		t.Fatalf("relay: %d %s", rec.Code, rec.Body)
	}
	out := logs.String()
	if !strings.Contains(out, "agent refused a request") || !strings.Contains(out, "status=409") || !strings.Contains(out, "code=linking") {
		t.Fatalf("refusal not logged:\n%s", out)
	}
	if strings.Contains(out, "Plans.pdf") {
		t.Fatalf("an error message reached the log:\n%s", out)
	}
}

func TestPageLog(t *testing.T) {
	_, a, logs := newLateAgent(t, 0, http.NotFoundHandler())
	b := New(context.Background(), a, &fakeHost{}, a.Log)
	rec := do(b, http.MethodPost, "/bridge/log", `{"method":"POST","path":"/v1/signin?x=secret","error":"TypeError: Failed to fetch"}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bridge/log: %d %s", rec.Code, rec.Body)
	}
	out := logs.String()
	if !strings.Contains(out, "page request failed") || !strings.Contains(out, "path=/v1/signin ") || !strings.Contains(out, "Failed to fetch") || strings.Contains(out, "secret") {
		t.Fatalf("page log line:\n%s", out)
	}
}

// The window's event stream when the agent starts late: the first attempt
// tells the page, a retry by the page then streams events.
func TestStreamAfterLateStart(t *testing.T) {
	la, a, _ := newLateAgent(t, 300*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			fmt.Fprint(w, `{}`)
		case "/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "id: 1\nevent: snapshot\ndata: {\"state\":{}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &fakeHost{}
	b := New(ctx, a, host, a.Log)
	b.FlushEvery = 10 * time.Millisecond
	if rec := do(b, http.MethodPost, "/bridge/stream", `{"window":"main"}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("stream: %d", rec.Code)
	}
	waitFor(t, func() bool {
		return strings.Contains(strings.Join(host.messages("main"), ""), "The connection to Olsyn Drive was lost.")
	})
	if la.starts.Load() != 1 {
		t.Fatalf("starts = %d", la.starts.Load())
	}
	if rec := do(b, http.MethodPost, "/bridge/stream", `{"window":"main"}`, nil); rec.Code != http.StatusAccepted {
		t.Fatalf("stream again: %d", rec.Code)
	}
	waitFor(t, func() bool { return strings.Contains(strings.Join(host.messages("main"), ""), "snapshot") })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
