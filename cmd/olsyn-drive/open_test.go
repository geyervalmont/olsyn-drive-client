package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// openAgent serves GET /v1/state and records POST /v1/open bodies.
func openAgent(t *testing.T) func() []agent.OpenAction {
	dir, err := os.MkdirTemp("", "odc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("OLSYN_DRIVE_HOME", dir)
	ep, _ := platform.Endpoint(platform.Under(dir))
	l, err := platform.Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []agent.OpenAction
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sampleState())
	})
	mux.HandleFunc("POST /v1/open", func(w http.ResponseWriter, r *http.Request) {
		var a agent.OpenAction
		json.NewDecoder(r.Body).Decode(&a)
		mu.Lock()
		got = append(got, a)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return func() []agent.OpenAction {
		mu.Lock()
		defer mu.Unlock()
		return append([]agent.OpenAction(nil), got...)
	}
}

func TestOpenScreen(t *testing.T) {
	opened := openAgent(t)
	cases := [][]string{
		{"open", "transfers", "held"},
		{"open", "transfers/waiting"},
		{"open", "transfers", "--filter", "skipped"},
		{"open", "--filter=done", "transfers"},
		{"open", "transfers"},
		{"open", "activity"},
		{"open", "settings"},
		{"open", "account"},
		{"open", "about"},
		{"open", "popover"},
	}
	want := []string{"transfers/held", "transfers/waiting", "transfers/skipped", "transfers/done", "transfers/active",
		"activity", "settings", "account", "about", "popover"}
	for _, args := range cases {
		if code, _, errOut := runCLI(t, args...); code != exitOK {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
	}
	got := opened()
	if len(got) != len(want) {
		t.Fatalf("opened %v", got)
	}
	for i, a := range got {
		if a.Target != "ui" || a.Path != want[i] {
			t.Errorf("%v: sent %+v, want ui %s", cases[i], a, want[i])
		}
	}
	for _, bad := range [][]string{{"open"}, {"open", "bogus"}, {"open", "transfers", "nope"}, {"open", "settings", "x"}, {"open", "--filter"}} {
		if code, _, _ := runCLI(t, bad...); code != exitUsage {
			t.Errorf("%v: exit %d, want usage", bad, code)
		}
	}
	if n := len(opened()); n != len(want) {
		t.Errorf("a refused command reached the agent (%d opens)", n)
	}
}

func TestParseOpenURLScreens(t *testing.T) {
	for link, want := range map[string]agent.OpenAction{
		"olsyn-drive://open/transfers/held": {Target: "ui", Path: "transfers/held"},
		"olsyn-drive://open/popover":        {Target: "ui", Path: "popover"},
		"olsyn-drive://show/about":          {Target: "ui", Path: "about"},
		"olsyn-drive://web?path=/Projects":  {Target: "web", Path: "/Projects"},
	} {
		got, err := parseOpenURL(link)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", link, got, err)
		}
	}
}
