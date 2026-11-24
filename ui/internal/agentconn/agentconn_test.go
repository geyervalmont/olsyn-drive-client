//go:build !windows

package agentconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAgent serves handler on a unix socket and returns a dialer for it.
func fakeAgent(t *testing.T, handler http.Handler) Dialer {
	t.Helper()
	dir, err := os.MkdirTemp("", "odui") // short: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return UnixDialer{Path: path}
}

func TestReadEvents(t *testing.T) {
	in := ": ping\n\nid: 7\nevent: summary\ndata: {\"active\":1}\n\n" +
		"event: state\r\ndata: {\"a\":\r\ndata: 2}\r\n\r\n" +
		"data: plain\n\n" +
		"event: partial\ndata: x" // no blank line: not dispatched
	var got []Event
	lines := 0
	err := ReadEvents(strings.NewReader(in), func() { lines++ }, func(e Event) error {
		got = append(got, e)
		return nil
	})
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("err = %v", err)
	}
	want := []Event{
		{ID: "7", Name: "summary", Data: `{"active":1}`},
		{ID: "7", Name: "state", Data: "{\"a\":\n2}"},
		{ID: "7", Name: "message", Data: "plain"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if lines < 9 {
		t.Fatalf("activity ran %d times", lines)
	}
}

func TestReadEventsStopsOnHandlerError(t *testing.T) {
	stop := errors.New("stop")
	err := ReadEvents(strings.NewReader("data: 1\n\ndata: 2\n\n"), nil, func(Event) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamSendsLastEventIDAndRefusesNon200(t *testing.T) {
	var gotID, gotOrigin string
	d := fakeAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" {
			http.NotFound(w, r)
			return
		}
		gotID, gotOrigin = r.Header.Get("Last-Event-ID"), r.Header.Get("Origin")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 42\nevent: snapshot\ndata: {\"state\":{\"connection\":\"online\"}}\n\n")
	}))
	client := NewHTTPClient(d)
	var events []Event
	opened := false
	err := Stream(context.Background(), client, "test", "41", func() { opened = true }, func(e Event) error {
		events = append(events, e)
		return nil
	})
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("err = %v", err)
	}
	if !opened || len(events) != 1 || events[0].ID != "42" || gotID != "41" || gotOrigin != "" {
		t.Fatalf("opened=%v events=%v lastID=%q origin=%q", opened, events, gotID, gotOrigin)
	}

	d2 := fakeAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	if err := Stream(context.Background(), NewHTTPClient(d2), "test", "", nil, func(Event) error { return nil }); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("non-200 err = %v", err)
	}
}

func TestAgentCallsAndErrors(t *testing.T) {
	d := fakeAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"connection":"online","paused":true,"transfers":{"active":2,"held":1}}`)
		case "/v1/pause":
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"not_signed_in","message":"Sign in first."}`)
		default:
			http.NotFound(w, r)
		}
	}))
	a := &Agent{HTTP: NewHTTPClient(d), UserAgent: "test"}
	s, err := a.GetState(context.Background())
	if err != nil || s.Connection != "online" || !s.Paused || s.Transfers.Held != 1 {
		t.Fatalf("state = %+v, %v", s, err)
	}
	err = a.Pause(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 || apiErr.Code != "not_signed_in" {
		t.Fatalf("pause err = %v", err)
	}
	if !a.Answering(context.Background()) {
		t.Fatal("agent should be answering")
	}
}

func TestEnsureRunningWithoutStarter(t *testing.T) {
	a := &Agent{HTTP: NewHTTPClient(UnixDialer{Path: filepath.Join(os.TempDir(), "odui-missing.sock")}), UserAgent: "test"}
	if a.Answering(context.Background()) {
		t.Fatal("nothing should answer")
	}
	if err := a.EnsureRunning(context.Background()); err == nil {
		t.Fatal("expected an error with no starter")
	}
}

func TestWatchFollowsSummaryAndReconnects(t *testing.T) {
	var mu sync.Mutex
	connects := 0
	var lastIDs []string
	d := fakeAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			fmt.Fprint(w, `{"connection":"online"}`)
		case "/v1/events":
			mu.Lock()
			connects++
			n := connects
			lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
			mu.Unlock()
			if n == 1 {
				fmt.Fprint(w, "id: 1\nevent: snapshot\ndata: {\"state\":{\"connection\":\"online\",\"account\":{\"name\":\"A\"}}}\n\n")
				fmt.Fprint(w, "id: 2\nevent: summary\ndata: {\"active\":3,\"files_left\":3}\n\n")
				fmt.Fprint(w, "id: 3\nevent: notification\ndata: {\"id\":\"n1\",\"kind\":\"held\",\"title\":\"T\"}\n\n")
				return // stream ends; Watch reconnects
			}
			fmt.Fprint(w, "id: 4\nevent: summary\ndata: {\"active\":0}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	a := &Agent{HTTP: NewHTTPClient(d), UserAgent: "test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	states := make(chan State, 8)
	notes := make(chan Notification, 2)
	go a.Watch(ctx, WatchHandlers{State: func(s State) { states <- s }, Notification: func(n Notification) { notes <- n }})

	var seen []int
	for len(seen) < 3 {
		select {
		case s := <-states:
			seen = append(seen, s.Transfers.Active)
		case <-ctx.Done():
			t.Fatalf("timed out; saw %v", seen)
		}
	}
	if fmt.Sprint(seen) != "[0 3 0]" {
		t.Fatalf("active counts %v", seen)
	}
	if n := <-notes; n.ID != "n1" {
		t.Fatalf("notification %+v", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lastIDs) < 2 || lastIDs[0] != "" || lastIDs[1] != "3" {
		t.Fatalf("Last-Event-ID per connect = %v", lastIDs)
	}
}
