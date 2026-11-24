package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// signedRelease returns a release answer (as the drive serves it) whose
// installer for windows-x64 is installer, signed with priv as key "test".
func signedRelease(t *testing.T, priv ed25519.PrivateKey, installer []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(installer)
	m := Manifest{Schema: 1, Product: Product, Channel: "stable", Version: "1.4.2", PublishedAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
		Notes: "synthetic test release",
		Artifacts: []Artifact{{Platform: "windows-x64", Role: "installer", File: "OlsynDriveSetup-1.4.2-x64.exe",
			URL: "/api/client/v1/artifacts/1.4.2/OlsynDriveSetup-1.4.2-x64.exe", Size: int64(len(installer)), SHA256: hex.EncodeToString(sum[:])}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{Channel: "stable", Manifest: base64.StdEncoding.EncodeToString(raw),
		Signature: Signature{Alg: "ed25519", KeyID: "test", Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw))}}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyInstaller(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := Keys{"test": pub}
	dir := t.TempDir()
	body := []byte("synthetic installer bytes")
	exe := filepath.Join(dir, "setup.exe")
	if err := os.WriteFile(exe, body, 0o600); err != nil {
		t.Fatal(err)
	}
	release := signedRelease(t, priv, body)

	m, a, err := VerifyInstaller(release, keys, "windows-x64", exe)
	if err != nil || m.Version != "1.4.2" || a.Role != "installer" {
		t.Fatalf("good release: %+v %+v %v", m, a, err)
	}

	// Another file than the one the signed manifest names.
	other := filepath.Join(dir, "other.exe")
	os.WriteFile(other, []byte("synthetic installer bytez"), 0o600)
	if _, _, err := VerifyInstaller(release, keys, "windows-x64", other); !errors.Is(err, ErrNotInstaller) {
		t.Fatalf("other file: %v", err)
	}
	// No installer for this platform.
	if _, _, err := VerifyInstaller(release, keys, "windows-arm64", exe); !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("other platform: %v", err)
	}
	// A manifest changed after signing.
	var env Envelope
	json.Unmarshal(release, &env)
	raw, _ := base64.StdEncoding.DecodeString(env.Manifest)
	raw[len(raw)-3] ^= 1
	env.Manifest = base64.StdEncoding.EncodeToString(raw)
	forged, _ := json.Marshal(env)
	if _, _, err := VerifyInstaller(forged, keys, "windows-x64", exe); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged manifest: %v", err)
	}
	// Signed by a key this build does not have.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if _, _, err := VerifyInstaller(release, Keys{"test": otherPub}, "windows-x64", exe); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: %v", err)
	}
	// A build with no keys says so.
	if _, _, err := VerifyInstaller(release, Keys{}, "windows-x64", exe); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("no keys: %v", err)
	}
	// Not a release answer at all.
	if _, _, err := VerifyInstaller([]byte("<html>"), keys, "windows-x64", exe); !errors.Is(err, ErrInvalid) {
		t.Fatalf("garbage: %v", err)
	}
}
