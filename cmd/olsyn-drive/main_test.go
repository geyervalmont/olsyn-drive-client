package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

func sampleState() agent.State {
	letter := "W:"
	eta := int64(58)
	return agent.State{
		APIVersion: 1, Version: "1.4.2", Platform: "windows-x64", Ready: true, Connection: agent.ConnWorking,
		Account:    &agent.Account{Server: "https://drive.example.test", Name: "Test Person", Email: "person@example.test"},
		DeviceName: "TEST-PC",
		Mount:      agent.Mount{State: agent.MountMounted, Letter: &letter, Path: `W:\`},
		Transfers:  agent.Summary{Active: 2, Waiting: 14, Held: 1, BytesTotal: 734003200, BytesDone: 123731968, UpBPS: 10485760, ETASeconds: &eta},
		Update:     agent.Update{State: "up_to_date"},
		Driver:     agent.Driver{State: "ok", Version: "2.2.25089"},
		Issues:     []agent.Issue{{Code: "held_files", Message: "1 file could not be saved to the drive."}},
	}
}

func TestFormatStatus(t *testing.T) {
	out := formatStatus(sampleState())
	for _, want := range []string{"Olsyn Drive 1.4.2", "Uploading", "<person@example.test> on drive.example.test", "W: mounted",
		"2 active, 14 waiting, 1 held", "WinFsp 2.2.25089", "1 file could not be saved"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestParseOpenURL(t *testing.T) {
	a, err := parseOpenURL("olsyn-drive://open?path=/Projects/Tower")
	if err != nil || a.Target != "drive" || a.Path != "/Projects/Tower" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := parseOpenURL("https://evil.example.test/"); err == nil {
		t.Fatal("accepted a non olsyn-drive link")
	}
	if _, err := parseOpenURL("olsyn-drive://format-c"); err == nil {
		t.Fatal("accepted an unknown action")
	}
}

func TestHealthContract(t *testing.T) {
	p := &agent.PendingUpdate{Version: "1.5.0", WasMounted: true}
	st := agent.State{Version: "1.5.0", Ready: true, Mount: agent.Mount{State: agent.MountMounted}}
	if err := healthy(st, p); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []agent.State{
		{Version: "1.4.2", Ready: true, Mount: agent.Mount{State: agent.MountMounted}},
		{Version: "1.5.0", Ready: false, Mount: agent.Mount{State: agent.MountMounted}},
		{Version: "1.5.0", Ready: true, Mount: agent.Mount{State: agent.MountFailed}},
	} {
		if healthy(bad, p) == nil {
			t.Errorf("unhealthy state passed: %+v", bad)
		}
	}
}

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	r, err := openRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 40)
	for range 10 {
		r.Write(line)
	}
	r.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 100 {
			t.Fatalf("%s: %v size %d", p, err, fi.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("kept more than 2 old files")
	}
}

// fakeAgent serves a few IPC endpoints on the real endpoint of OLSYN_DRIVE_HOME.
func fakeAgent(t *testing.T) (signouts *atomic.Int32) {
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
	signouts = &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sampleState())
	})
	mux.HandleFunc("POST /v1/signout", func(w http.ResponseWriter, r *http.Request) {
		signouts.Add(1)
		var body map[string]bool
		json.NewDecoder(r.Body).Decode(&body)
		if body["clear_cache"] && !body["discard_unsent_confirmed"] {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"unsent_data","message":"2 files (1.0 MB) have not reached the drive yet.","unsent":{"files":2}}`))
			return
		}
		json.NewEncoder(w).Encode(agent.SignOutResult{SignedOut: true, Unlinked: true, CacheCleared: body["clear_cache"]})
	})
	mux.HandleFunc("POST /v1/pause", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sampleState())
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return signouts
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := run(ctx, args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIAgainstAgent(t *testing.T) {
	signouts := fakeAgent(t)
	code, out, _ := runCLI(t, "status", "--json")
	if code != exitOK {
		t.Fatalf("status --json exit %d", code)
	}
	var st agent.State
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Version != "1.4.2" {
		t.Fatalf("status --json output: %v\n%s", err, out)
	}
	code, out, _ = runCLI(t, "status")
	if code != exitOK || !strings.Contains(out, "W: mounted") {
		t.Fatalf("status: %d\n%s", code, out)
	}
	// Clearing the cache with unsent files needs confirmation; stdin is not a terminal.
	code, _, errOut := runCLI(t, "logout", "--clear-cache")
	if code != exitNeedsInput || !strings.Contains(errOut, "have not reached the drive") {
		t.Fatalf("logout --clear-cache: %d %s", code, errOut)
	}
	code, out, _ = runCLI(t, "logout", "--clear-cache", "--yes")
	if code != exitOK || !strings.Contains(out, "cache was deleted") {
		t.Fatalf("logout --yes: %d %s", code, out)
	}
	if signouts.Load() != 2 {
		t.Fatalf("signout calls = %d", signouts.Load())
	}
	if code, _, _ := runCLI(t, "pause", "--minutes", "5"); code != exitOK {
		t.Fatalf("pause: %d", code)
	}
}

// The installed program is gone (security software removed it) and the
// agent does not answer: the CLI says so in plain words, with the help page,
// instead of "not running".
func TestCLINamesARemovedProgram(t *testing.T) {
	dir, err := os.MkdirTemp("", "odc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("OLSYN_DRIVE_HOME", dir)
	rec := blocked.Record{Dir: platform.Under(dir).State}
	if err := rec.NoteInstalled(filepath.Join(dir, "Programs", "Olsyn Drive", platform.ExecutableName)); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "status")
	if code != exitNoAgent {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"removed or blocked Olsyn Drive", "Your files waiting to upload are safe.", blocked.HelpURL} {
		if !strings.Contains(errOut, want) {
			t.Errorf("status lacks %q:\n%s", want, errOut)
		}
	}
	code, out, _ := runCLI(t, "status", "--json")
	var js map[string]string
	if code != exitNoAgent || json.Unmarshal([]byte(out), &js) != nil || js["blocked"] != "removed" || js["help_url"] != blocked.HelpURL {
		t.Fatalf("status --json: %d %s", code, out)
	}
	// After an uninstall the program is gone on purpose: plain "not running".
	rec.ForgetInstalled()
	if _, _, errOut := runCLI(t, "status"); !strings.Contains(errOut, "not running") {
		t.Fatalf("after uninstall: %s", errOut)
	}
}

func TestCLINoAgent(t *testing.T) {
	dir, err := os.MkdirTemp("", "odc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("OLSYN_DRIVE_HOME", dir)
	code, out, _ := runCLI(t, "status", "--json")
	if code != exitNoAgent || !strings.Contains(out, "not_running") {
		t.Fatalf("status without agent: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, "bogus"); code != exitUsage {
		t.Fatalf("unknown verb exit %d", code)
	}
	if code, out, _ := runCLI(t, "version"); code != exitOK || !strings.Contains(out, "olsyn-drive dev") {
		t.Fatalf("version: %d %s", code, out)
	}
}
