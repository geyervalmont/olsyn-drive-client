//go:build !windows

package agentconn

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type delayedListen struct {
	t      *testing.T
	sock   string
	delay  time.Duration
	h      http.Handler
	starts atomic.Int32
}

func (d *delayedListen) Start() error {
	if d.starts.Add(1) > 1 {
		return nil
	}
	go func() {
		time.Sleep(d.delay)
		ln, err := net.Listen("unix", d.sock)
		if err != nil {
			d.t.Errorf("listen: %v", err)
			return
		}
		srv := &http.Server{Handler: d.h}
		d.t.Cleanup(func() { srv.Close() })
		go srv.Serve(ln)
	}()
	return nil
}

// The shell's own stream when the agent is not running at first and starts
// 50-500 ms after being launched: Watch must start it once and then follow it.
func TestWatchAgentStartsLate(t *testing.T) {
	for _, delay := range []time.Duration{50 * time.Millisecond, 500 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "odw")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			d := &delayedListen{t: t, sock: filepath.Join(dir, "a.sock"), delay: delay, h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/state":
					fmt.Fprint(w, `{"connection":"signed_out"}`)
				case "/v1/events":
					fmt.Fprint(w, "id: 1\nevent: snapshot\ndata: {\"state\":{\"connection\":\"signed_out\"}}\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			})}
			a := &Agent{HTTP: NewHTTPClient(UnixDialer{Path: d.sock}), UserAgent: "test", Starter: d}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			states := make(chan State, 4)
			go a.Watch(ctx, WatchHandlers{State: func(s State) { states <- s }})
			select {
			case s := <-states:
				if s.Connection != "signed_out" {
					t.Fatalf("state %+v", s)
				}
			case <-ctx.Done():
				t.Fatal("Watch never reached the late agent")
			}
			if n := d.starts.Load(); n != 1 {
				t.Fatalf("started %d times", n)
			}
		})
	}
}

func TestNotRunning(t *testing.T) {
	c := NewHTTPClient(UnixDialer{Path: filepath.Join(os.TempDir(), "odw-none.sock")})
	_, err := c.Get(BaseURL + "/v1/state")
	if !NotRunning(err) {
		t.Fatalf("NotRunning(%v) = false", err)
	}
}
