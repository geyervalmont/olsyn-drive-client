//go:build devbuild

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func TestOnlyLoopback(t *testing.T) {
	for _, a := range []string{"127.0.0.1:8787", "[::1]:9", "localhost:1"} {
		if err := loopback(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"0.0.0.0:8787", ":8787", "192.0.2.10:80", "example.test:80"} {
		if err := loopback(a); err == nil {
			t.Errorf("%s accepted", a)
		}
	}
	if _, err := parse([]string{"--addr", "0.0.0.0:1"}, io.Discard); err == nil {
		t.Fatal("parse took a public address")
	}
}

func TestSeedAndFaults(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{})
	defer fd.Close()
	n := seed(fd, 50, 3<<20)
	st := fd.Stats()
	if st.Files != n.files || st.Folders != n.folders || n.files < 60 || st.Bytes < 3<<20 {
		t.Fatalf("seeded %+v, stats %+v", n, st)
	}
	if fi, ok := fd.Lookup("/Large file/Generated 3 MiB.bin"); !ok || !bytes.Equal(fi.Data, nil) {
		t.Fatalf("virtual file: %v", ok)
	}
	if len(fd.List(mustID(t, fd, "/Large folder"))) != 50 {
		t.Fatal("large folder")
	}

	srv := httptest.NewServer(faults(fd, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	post := func(p string) int {
		res, err := http.Post(srv.URL+p, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if post("/deploy?seconds=2") != 200 || post("/deploy?seconds=x") != 400 || post("/offline?on=maybe") != 400 {
		t.Fatal("deploy/offline arguments")
	}
	res, err := http.Get(fd.URL() + "/api/v1/me")
	if err != nil || res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") == "" {
		t.Fatalf("deploy not in effect: %v %v", err, res)
	}
	res.Body.Close()
	if post("/deploy?seconds=0") != 200 || post("/revoke") != 200 {
		t.Fatal("controls")
	}
	req, _ := http.NewRequest(http.MethodGet, fd.URL()+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+fd.Token())
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoke not in effect: %v %v", err, res)
	}
	res.Body.Close()
	if post("/offline?on=1") != 200 {
		t.Fatal("offline")
	}
	if _, err := http.Get(fd.URL() + "/link"); err == nil {
		t.Fatal("answered while offline")
	}
	post("/offline?on=0")
	res, err = http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	var got fakedrive.Stats
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil || got.Files != n.files {
		t.Fatalf("stats %v %+v", err, got)
	}
	res.Body.Close()
}

func TestRunStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--addr", "127.0.0.1:0", "--faults", "127.0.0.1:0", "--big-folder", "5", "--virtual-gib", "0"}, &out, &errb)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "synthetic data only") || strings.Contains(errb.String(), "odt_") {
		t.Fatalf("output %q / %q", out.String(), errb.String())
	}
}

func mustID(t *testing.T, fd *fakedrive.Server, p string) string {
	fi, ok := fd.Lookup(p)
	if !ok {
		t.Fatalf("%s missing", p)
	}
	return fi.ID
}
