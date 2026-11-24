package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testPlatform = "windows-x64"

// testKey is a fixed key used only by these tests.
func testKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
}

type fakeServer struct {
	mu       sync.Mutex
	envelope []byte
	artifact []byte
	srv      *httptest.Server
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/client/v1/releases/{channel}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.envelope == nil {
			http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(f.envelope)
	})
	mux.HandleFunc("GET /api/client/v1/artifacts/{version}/{file}", func(w http.ResponseWriter, r *http.Request) {
		// The real server redirects to a presigned URL.
		http.Redirect(w, r, "/blob/"+r.PathValue("file"), http.StatusFound)
	})
	mux.HandleFunc("GET /blob/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(f.artifact)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// publish signs a manifest for version with the given artifact bytes. The
// manifest's sha256/size describe declared, which may differ from served.
func (f *fakeServer) publish(t *testing.T, key ed25519.PrivateKey, m Manifest, served []byte) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	env := Envelope{
		Channel:  m.Channel,
		Manifest: base64.StdEncoding.EncodeToString(raw),
		Signature: Signature{Alg: "ed25519", KeyID: "test1",
			Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))},
	}
	data, _ := json.Marshal(env)
	f.mu.Lock()
	f.envelope = data
	f.artifact = served
	f.mu.Unlock()
}

func manifestFor(version string, published time.Time, body []byte) Manifest {
	sum := sha256.Sum256(body)
	return Manifest{
		Schema: 1, Product: Product, Channel: "stable", Version: version, PublishedAt: published,
		Notes: "synthetic test release",
		Artifacts: []Artifact{
			{Platform: testPlatform, Role: "installer", File: "OlsynDriveSetup-" + version + "-x64.exe",
				URL: "/api/client/v1/artifacts/" + version + "/OlsynDriveSetup-" + version + "-x64.exe", Size: 3, SHA256: hex.EncodeToString(make([]byte, 32))},
			{Platform: testPlatform, Role: "update", File: "olsyn-drive-" + version + "-x64.exe",
				URL: "/api/client/v1/artifacts/" + version + "/olsyn-drive-" + version + "-x64.exe", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])},
		},
	}
}

func newClient(t *testing.T, f *fakeServer, current string) *Client {
	dir := t.TempDir()
	return &Client{
		HTTP:       f.srv.Client(),
		BaseURL:    f.srv.URL,
		Keys:       Keys{"test1": testKey().Public().(ed25519.PublicKey)},
		Platform:   testPlatform,
		Current:    current,
		Store:      NewStore(filepath.Join(dir, "update.json")),
		StagingDir: filepath.Join(dir, "staging"),
	}
}

var t0 = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func TestGoodUpdate(t *testing.T) {
	f := newFakeServer(t)
	body := []byte("new program bytes v1.5.0")
	f.publish(t, testKey(), manifestFor("1.5.0", t0, body), body)
	c := newClient(t, f, "1.4.2")
	ctx := context.Background()

	rel, err := c.Check(ctx, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Newer || rel.Required || rel.Artifact == nil || rel.Artifact.Role != "update" {
		t.Fatalf("release = %+v", rel)
	}
	var lastDone int64
	staged, err := c.Download(ctx, rel, func(done, total int64) { lastDone = done })
	if err != nil {
		t.Fatal(err)
	}
	if lastDone != int64(len(body)) {
		t.Fatalf("progress ended at %d", lastDone)
	}
	got, _ := os.ReadFile(staged)
	if !bytes.Equal(got, body) {
		t.Fatal("staged bytes differ")
	}

	exe := filepath.Join(t.TempDir(), "olsyn-drive.exe")
	os.WriteFile(exe, []byte("old program"), 0o755)
	swap, err := Install(exe, staged)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, body) {
		t.Fatal("new program not in place")
	}
	if b, _ := os.ReadFile(swap.Old); string(b) != "old program" {
		t.Fatal("old program not kept")
	}
	started := 0
	h := Handoff{
		Start:   func() (Process, error) { started++; return &fakeProc{}, nil },
		Healthy: func(context.Context) error { return nil },
		Version: "1.5.0",
		Timeout: time.Second,
		Every:   10 * time.Millisecond,
	}
	if err := h.Run(ctx, swap); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("started %d times", started)
	}
	if err := CleanupOld(exe); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(swap.Old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".old not cleaned up")
	}
}

func TestNoRelease(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f, "1.4.2")
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("err = %v", err)
	}
}

func TestBadSignature(t *testing.T) {
	f := newFakeServer(t)
	body := []byte("x")
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	f.publish(t, other, manifestFor("1.5.0", t0, body), body) // signed by a key we do not trust under id test1
	c := newClient(t, f, "1.4.2")
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v", err)
	}

	// Tampered bytes under a valid signature also fail.
	f.publish(t, testKey(), manifestFor("1.5.0", t0, body), body)
	var env Envelope
	json.Unmarshal(f.envelope, &env)
	raw, _ := base64.StdEncoding.DecodeString(env.Manifest)
	raw = bytes.Replace(raw, []byte("1.5.0"), []byte("9.9.9"), 1)
	env.Manifest = base64.StdEncoding.EncodeToString(raw)
	f.envelope, _ = json.Marshal(env)
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered: err = %v", err)
	}

	// Unknown key id.
	env.Signature.KeyID = "nope"
	f.envelope, _ = json.Marshal(env)
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: err = %v", err)
	}
}

func TestWrongChannelRefused(t *testing.T) {
	f := newFakeServer(t)
	m := manifestFor("1.5.0", t0, []byte("x"))
	m.Channel = "beta"
	f.publish(t, testKey(), m, nil)
	c := newClient(t, f, "1.4.2")
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplayedOlderManifest(t *testing.T) {
	f := newFakeServer(t)
	body := []byte("x")
	c := newClient(t, f, "1.4.2")
	f.publish(t, testKey(), manifestFor("1.6.0", t0.Add(48*time.Hour), body), body)
	if _, err := c.Check(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	// An attacker (or a stale cache) serves the older, validly signed manifest.
	f.publish(t, testKey(), manifestFor("1.5.0", t0, body), body)
	if _, err := c.Check(context.Background(), "stable"); !errors.Is(err, ErrReplayed) {
		t.Fatalf("err = %v", err)
	}
	// The same manifest again is fine.
	f.publish(t, testKey(), manifestFor("1.6.0", t0.Add(48*time.Hour), body), body)
	if _, err := c.Check(context.Background(), "stable"); err != nil {
		t.Fatalf("same manifest again: %v", err)
	}
}

func TestHashMismatch(t *testing.T) {
	f := newFakeServer(t)
	declared := []byte("the real bytes")
	served := []byte("evil bytes 1234") // different content
	f.publish(t, testKey(), manifestFor("1.5.0", t0, declared), served)
	c := newClient(t, f, "1.4.2")
	rel, err := c.Check(context.Background(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Download(context.Background(), rel, nil); !errors.Is(err, ErrHashMismatch) && !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(c.StagingDir, "1.5.0"))
	if len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v", entries)
	}
	// Same size, different bytes: hash check.
	served = []byte("the evil bytes")
	f.publish(t, testKey(), manifestFor("1.5.0", t0, declared), served)
	if _, err := c.Download(context.Background(), rel, nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("same size: err = %v", err)
	}
}

type fakeProc struct{ killed atomic.Bool }

func (p *fakeProc) Kill() error { p.killed.Store(true); return nil }

func TestFailedHealthCheckRollsBack(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "olsyn-drive.exe")
	staged := filepath.Join(dir, "staged.exe")
	os.WriteFile(exe, []byte("old program"), 0o755)
	os.WriteFile(staged, []byte("broken new program"), 0o755)
	swap, err := Install(exe, staged)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(dir, "update.json"))
	proc := &fakeProc{}
	restarted := false
	h := Handoff{
		Start:   func() (Process, error) { return proc, nil },
		Healthy: func(context.Context) error { return errors.New("agent not answering") },
		Restart: func() error { restarted = true; return nil },
		Store:   store,
		Version: "1.5.0",
		Timeout: 100 * time.Millisecond,
		Every:   10 * time.Millisecond,
	}
	err = h.Run(context.Background(), swap)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	if !proc.killed.Load() || !restarted {
		t.Fatalf("killed=%v restarted=%v", proc.killed.Load(), restarted)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old program" {
		t.Fatalf("exe = %q after rollback", b)
	}
	if reason, ok := store.Failed("1.5.0"); !ok || reason == "" {
		t.Fatal("failure not recorded")
	}
}

func TestMinSupportedAndDriver(t *testing.T) {
	f := newFakeServer(t)
	body := []byte("x")
	m := manifestFor("2.0.0", t0, body)
	m.MinSupported = "1.5.0"
	m.Driver = &Driver{Name: "winfsp", MinVersion: "2.1"}
	f.publish(t, testKey(), m, body)
	c := newClient(t, f, "1.4.2")
	rel, err := c.Check(context.Background(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Required || !rel.Newer {
		t.Fatalf("release = %+v", rel)
	}
	if DriverAtLeast("2.0.24051", rel.Manifest.Driver.MinVersion) {
		t.Fatal("2.0 should not satisfy 2.1")
	}
	if !DriverAtLeast("2.2.25089", rel.Manifest.Driver.MinVersion) {
		t.Fatal("2.2 should satisfy 2.1")
	}
	if DriverAtLeast("", "2.0") {
		t.Fatal("missing driver satisfies nothing")
	}
}

func TestRollbackManifestOffersOlderVersion(t *testing.T) {
	f := newFakeServer(t)
	body := []byte("x")
	m := manifestFor("1.4.1", t0, body)
	m.Rollback = true
	f.publish(t, testKey(), m, body)
	c := newClient(t, f, "1.4.2")
	rel, err := c.Check(context.Background(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Newer {
		t.Fatal("signed rollback should be offered")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.4.2", "1.4.2", 0},
		{"1.4.10", "1.4.9", 1},
		{"1.5.0-beta.1", "1.5.0", -1},
		{"1.5.0-beta.2", "1.5.0-beta.10", -1},
		{"v2.0.0", "1.99.99", 1},
	}
	for _, c := range cases {
		got, err := CompareStrings(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("Compare(%s, %s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	if _, err := ParseVersion("dev"); err == nil {
		t.Error("dev parsed as a version")
	}
}

func TestNoKeysRefusesEverything(t *testing.T) {
	if _, err := Verify(Envelope{}, Keys{}, "stable"); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v", err)
	}
	keys, err := ReleaseKeys()
	if err != nil {
		t.Fatal(err)
	}
	_ = keys
}
