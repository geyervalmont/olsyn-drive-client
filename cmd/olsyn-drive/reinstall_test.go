//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// What happened on a Windows test PC, simulated with real processes: an
// upload is in flight when security software kills the agent and deletes
// the program (and its registrations). The program is installed again;
// the upload carries on from the journal and cache in the data folder and
// finishes, and in between the CLI names what happened.
func TestUploadFinishesAfterProgramRemovedAndReinstalled(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program and runs it")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	work := t.TempDir()
	pristine := filepath.Join(work, "build", "olsyn-drive")
	build := exec.Command(goTool, "build", "-tags", "devbuild", "-o", pristine, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	fd := fakedrive.New(fakedrive.Options{AutoApprove: true, PartSize: 1 << 20, SingleMax: 1 << 20})
	defer fd.Close()
	fd.AddFolder(fd.RootID(), "Incoming")

	// A short data folder: unix socket paths are limited to about 100 bytes.
	home, err := os.MkdirTemp("", "odr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("OLSYN_DRIVE_HOME", home)
	paths := platform.Under(home)
	installed := filepath.Join(work, "Programs", "Olsyn Drive", platform.ExecutableName)
	install := func() {
		t.Helper()
		if err := installFile(pristine, installed); err != nil {
			t.Fatal(err)
		}
		if err := (blocked.Record{Dir: paths.State}).NoteInstalled(installed); err != nil {
			t.Fatal(err)
		}
	}
	ep, err := platform.Endpoint(paths)
	if err != nil {
		t.Fatal(err)
	}
	client := agent.NewClient(func(ctx context.Context) (net.Conn, error) { return platform.Dial(ctx, ep) })
	waitFor := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	state := func() (agent.State, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		st, err := client.State(ctx)
		return st, err == nil
	}
	var logs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent output:\n%s", logs.String())
		}
	})
	startAgent := func() *exec.Cmd {
		t.Helper()
		cmd := exec.Command(installed, "agent", "--server", fd.URL())
		cmd.Env = append(os.Environ(), "OLSYN_DRIVE_HOME="+home, "OLSYN_DRIVE_TOKEN_STORE=file")
		cmd.Stdout, cmd.Stderr = &logs, &logs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor("the agent to answer", 15*time.Second, func() bool { _, ok := state(); return ok })
		return cmd
	}

	install()
	first := startAgent()
	defer func() { first.Process.Kill(); first.Wait() }()
	if err := client.Do(context.Background(), http.MethodPost, "/v1/signin", map[string]bool{"open_browser": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor("signed in", 15*time.Second, func() bool {
		st, ok := state()
		return ok && st.Account != nil && st.Ready
	})
	// Slow uploads down so the kill lands mid-file.
	if err := client.Do(context.Background(), http.MethodPatch, "/v1/settings",
		map[string]any{"bandwidth": map[string]int64{"up_bps": 2 << 20}}, nil); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 12<<20)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(work, "src", "model.bin")
	os.MkdirAll(filepath.Dir(src), 0o700)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "send", src, "--to", "/Incoming"); code != exitOK {
		t.Fatalf("send: %d %s", code, errOut)
	}
	waitFor("part of the upload", 30*time.Second, func() bool {
		st, ok := state()
		return ok && st.Transfers.BytesDone >= 2<<20
	})

	// Security software: the agent is killed and its program deleted.
	first.Process.Kill()
	first.Wait()
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if _, ok := fd.Lookup("/Incoming/model.bin"); ok {
		t.Fatal("the upload finished before the kill; the test proves nothing")
	}
	code, _, errOut := runCLI(t, "status")
	if code != exitNoAgent || !strings.Contains(errOut, "removed or blocked Olsyn Drive") || !strings.Contains(errOut, "safe") {
		t.Fatalf("status after removal: %d %s", code, errOut)
	}

	// Installed again: the upload carries on and finishes, without signing in again.
	install()
	second := startAgent()
	defer func() { second.Process.Kill(); second.Wait() }()
	want := sha256.Sum256(data)
	waitFor("the upload to finish", 60*time.Second, func() bool {
		f, ok := fd.Lookup("/Incoming/model.bin")
		return ok && len(f.Data) == len(data) && sha256.Sum256(f.Data) == want
	})
	if st, ok := state(); !ok || st.Account == nil {
		t.Fatalf("not signed in after the reinstall: %+v", st.Connection)
	}
}
