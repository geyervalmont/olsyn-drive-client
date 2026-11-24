//go:build devbuild

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// The routes the install scripts use, as the manual test in
// docs/testing-windows.md drives them.
func TestDevRelease(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{})
	defer fd.Close()
	dir := t.TempDir()
	exe := filepath.Join(dir, "olsyn-drive.exe")
	os.WriteFile(exe, []byte("synthetic installer"), 0o600)
	scripts := filepath.Join(dir, "scripts")
	os.Mkdir(scripts, 0o700)
	os.WriteFile(filepath.Join(scripts, "install.ps1"), []byte("$DriveBase = '__DRIVE_BASE__'\n"), 0o600)
	rel, err := newDevRelease(fd, exe, "1.0.0-dev", scripts)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(fd.URL() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	_, manifest := get("/api/client/v1/releases/stable.json")
	keys, err := update.ParseKeys(map[string]string{rel.keyID: rel.publicKey})
	if err != nil {
		t.Fatal(err)
	}
	m, a, err := update.VerifyInstaller(manifest, keys, "windows-arm64", exe)
	if err != nil || m.Version != "1.0.0-dev" {
		t.Fatalf("manifest: %+v %v", m, err)
	}

	// The artifact URL redirects to storage (the client follows it).
	resp, body := get(a.URL)
	if resp.StatusCode != 200 || string(body) != "synthetic installer" || resp.Request.URL.Path != "/_dev/storage/"+a.File {
		t.Fatalf("artifact: %d %q via %s", resp.StatusCode, body, resp.Request.URL)
	}
	if resp, _ := get("/api/client/v1/artifacts/1.0.0-dev/other.exe"); resp.StatusCode != 404 {
		t.Fatalf("unknown artifact: %d", resp.StatusCode)
	}

	_, script := get("/install.ps1")
	if want := "$DriveBase = '" + fd.URL() + "'"; !strings.Contains(string(script), want) {
		t.Fatalf("script not filled in: %s", script)
	}
	// The rest of the fake drive still answers.
	if resp, _ := get("/api/client/v1/releases/beta.json"); resp.StatusCode != 404 {
		t.Fatalf("beta: %d", resp.StatusCode)
	}
	var env update.Envelope
	if err := json.Unmarshal(manifest, &env); err != nil || env.Signature.KeyID != "dev" {
		t.Fatalf("envelope: %+v %v", env, err)
	}
}
