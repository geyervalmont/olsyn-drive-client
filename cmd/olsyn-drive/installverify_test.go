package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// release signs a manifest whose windows-x64 installer is the file at exe.
func release(t *testing.T, priv ed25519.PrivateKey, exe string) []byte {
	t.Helper()
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	raw, _ := json.Marshal(update.Manifest{Schema: 1, Product: update.Product, Channel: "stable", Version: "1.4.2",
		PublishedAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
		Artifacts: []update.Artifact{{Platform: "windows-x64", Role: "installer", File: "OlsynDriveSetup-1.4.2-x64.exe",
			URL: "/api/client/v1/artifacts/1.4.2/OlsynDriveSetup-1.4.2-x64.exe", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}}})
	out, _ := json.Marshal(update.Envelope{Channel: "stable", Manifest: base64.StdEncoding.EncodeToString(raw),
		Signature: update.Signature{Alg: "ed25519", KeyID: "test", Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))}})
	return out
}

// The installer's own check of the manifest the install script hands it.
func TestInstallManifestVerification(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	keys := update.Keys{"test": pub}
	dir := t.TempDir()
	exe := filepath.Join(dir, "OlsynDriveSetup.exe")
	os.WriteFile(exe, []byte("synthetic installer"), 0o600)
	good := release(t, priv, exe)

	if m, err := verifyInstallManifest(good, keys, "windows-x64", exe, false); err != nil || m.Version != "1.4.2" {
		t.Fatalf("good manifest refused: %v", err)
	}
	// A different file than the signed manifest names (the script was handed something else).
	swapped := filepath.Join(dir, "swapped.exe")
	os.WriteFile(swapped, []byte("synthetic installer, changed"), 0o600)
	if _, err := verifyInstallManifest(good, keys, "windows-x64", swapped, true); !errors.Is(err, update.ErrNotInstaller) {
		t.Fatalf("swapped file: %v", err)
	}
	// A manifest signed by someone else is refused even with --allow-unverified.
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	if _, err := verifyInstallManifest(release(t, otherPriv, exe), keys, "windows-x64", exe, true); !errors.Is(err, update.ErrBadSignature) {
		t.Fatalf("foreign signature: %v", err)
	}
	// A build without keys refuses, unless told to carry on.
	if _, err := verifyInstallManifest(good, update.Keys{}, "windows-x64", exe, false); !errors.Is(err, errUnverified) {
		t.Fatalf("no keys: %v", err)
	}
	if _, err := verifyInstallManifest(good, update.Keys{}, "windows-x64", exe, true); !errors.Is(err, update.ErrNoKeys) {
		t.Fatalf("no keys, allowed: %v", err)
	}
}

// "install --manifest" refuses before touching anything when the manifest
// does not check out.
func TestInstallRefusesBadManifest(t *testing.T) {
	home, err := os.MkdirTemp("", "odc-") // short: the agent socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("OLSYN_DRIVE_HOME", home)
	t.Setenv("OLSYN_DRIVE_DEV_RELEASE_KEYS", "")
	bad := filepath.Join(t.TempDir(), "stable.json")
	os.WriteFile(bad, []byte("<html>not a release</html>"), 0o600)
	code, _, errOut := runCLI(t, "install", "--user", "--quiet", "--manifest", bad)
	if code != exitError {
		t.Fatalf("exit %d, want %d: %s", code, exitError, errOut)
	}
	if !strings.Contains(errOut, "release") {
		t.Fatalf("message: %s", errOut)
	}
}
