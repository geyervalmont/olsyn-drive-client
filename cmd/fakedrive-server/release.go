//go:build devbuild

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// devRelease serves what the install scripts need, for trying them by hand
// against this fake drive (docs/testing-windows.md, "Install scripts"):
//
//	GET /api/client/v1/releases/stable.json   a manifest signed with a key made at start
//	GET /api/client/v1/artifacts/<v>/<file>   302 to /_dev/storage/<file>, as the drive redirects to storage
//	GET /_dev/storage/<file>                  the installer bytes
//	GET /install.ps1, /install.cmd            the scripts, with the server's address filled in
type devRelease struct {
	exe       string // the installer served for windows-x64 and windows-arm64
	file      string // its published file name
	scriptDir string // folder holding install.ps1 and install.cmd
	base      string // this server's address
	keyID     string
	publicKey string // base64, for OLSYN_DRIVE_DEV_RELEASE_KEYS
}

// newDevRelease signs a manifest for exe and registers it on fd.
func newDevRelease(fd *fakedrive.Server, exe, version, scriptDir string) (*devRelease, error) {
	if _, err := update.ParseVersion(version); err != nil {
		return nil, fmt.Errorf("-release-version %q: %w", version, err)
	}
	size, sum, err := digest(exe)
	if err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("make a release key: %w", err)
	}
	d := &devRelease{exe: exe, file: "OlsynDriveSetup-" + version + ".exe", scriptDir: scriptDir, base: fd.URL(), keyID: "dev",
		publicKey: base64.StdEncoding.EncodeToString(pub)}
	artifact := func(platform string) update.Artifact {
		return update.Artifact{Platform: platform, Role: "installer", File: d.file,
			URL: "/api/client/v1/artifacts/" + version + "/" + d.file, Size: size, SHA256: sum}
	}
	m := update.Manifest{Schema: 1, Product: update.Product, Channel: "stable", Version: version,
		PublishedAt: time.Now().UTC().Truncate(time.Millisecond), Notes: "Development release served by fakedrive-server.",
		Artifacts: []update.Artifact{artifact("windows-x64"), artifact("windows-arm64")}}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	fd.SetRelease("stable", fakedrive.ReleaseBody("stable", raw, d.keyID, ed25519.Sign(priv, raw)))
	fd.SetIntercept(d.serve)
	return d, nil
}

func digest(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open the release exe: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("read the release exe: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// serve answers the release routes the fake drive does not have; false
// passes the request on.
func (d *devRelease) serve(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch p := r.URL.Path; {
	case p == "/install.ps1" || p == "/install.cmd":
		if d.scriptDir == "" {
			http.Error(w, "start fakedrive-server with -install-scripts <folder>", http.StatusNotFound)
			return true
		}
		body, err := os.ReadFile(filepath.Join(d.scriptDir, strings.TrimPrefix(p, "/")))
		if err != nil {
			http.Error(w, "script not found", http.StatusNotFound)
			return true
		}
		body = bytes.ReplaceAll(body, []byte("__DRIVE_BASE__"), []byte(d.base))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
		return true
	case strings.HasPrefix(p, "/api/client/v1/artifacts/"):
		if filepath.Base(p) != d.file {
			http.Error(w, "not a file of a published release", http.StatusNotFound)
			return true
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/_dev/storage/"+d.file, http.StatusFound)
		return true
	case p == "/_dev/storage/"+d.file:
		f, err := os.Open(d.exe)
		if err != nil {
			http.Error(w, "release exe gone", http.StatusNotFound)
			return true
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, "release exe unreadable", http.StatusInternalServerError)
			return true
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, d.file, fi.ModTime(), f)
		return true
	}
	return false
}
