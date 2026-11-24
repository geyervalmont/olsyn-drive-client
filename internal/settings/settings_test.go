package settings

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testEnv(t *testing.T) Env {
	return Env{CacheDir: filepath.Join(t.TempDir(), "cache"), FreeDiskBytes: 100 * uint64(GiB)}
}

func TestDefaultCacheCap(t *testing.T) {
	cases := []struct {
		free uint64
		want int64
	}{
		{1000 * uint64(GiB), 50 * GiB}, // capped at 50 GiB
		{100 * uint64(GiB), 20 * GiB},  // 20%
		{2 * uint64(GiB), MinCacheCap}, // floor
		{0, MinCacheCap},
	}
	for _, c := range cases {
		if got := DefaultCacheCap(c.free); got != c.want {
			t.Errorf("DefaultCacheCap(%d) = %d, want %d", c.free, got, c.want)
		}
	}
}

func TestDefaults(t *testing.T) {
	env := testEnv(t)
	s := Defaults(env)
	if s.DriveLetter != "W:" || !s.StartAtLogin || s.UpdateChannel != "stable" || s.CacheCapBytes != 20*GiB {
		t.Fatalf("defaults = %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingAndRoundTrip(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	s, created, err := Load(path, env)
	if err != nil || !created {
		t.Fatalf("load missing: created=%v err=%v", created, err)
	}
	s.DriveLetter = "X:"
	s.Bandwidth.UpBPS = 1_000_000
	s.Notifications.Finished = false
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, created, err := Load(path, env)
	if err != nil || created {
		t.Fatalf("load: created=%v err=%v", created, err)
	}
	if got != s {
		t.Fatalf("round trip: got %+v want %+v", got, s)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("file has no version: %s", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"version":1,"drive_letter":"y"}`), 0o600)
	s, _, err := Load(path, env)
	if err != nil {
		t.Fatal(err)
	}
	if s.DriveLetter != "Y:" || s.UpdateChannel != "stable" || s.CacheDir != env.CacheDir {
		t.Fatalf("partial load = %+v", s)
	}
}

func TestLoadNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"version":99}`), 0o600)
	if _, _, err := Load(path, testEnv(t)); !errors.Is(err, ErrNewerVersion) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	base := Defaults(testEnv(t))
	cases := map[string]func(*Settings){
		"drive_letter":     func(s *Settings) { s.DriveLetter = "C:" },
		"cache_dir":        func(s *Settings) { s.CacheDir = "relative" },
		"cache_cap_bytes":  func(s *Settings) { s.CacheCapBytes = 10 },
		"bandwidth.up_bps": func(s *Settings) { s.Bandwidth.UpBPS = -1 },
		"update_channel":   func(s *Settings) { s.UpdateChannel = "nightly" },
		"server_url":       func(s *Settings) { s.ServerURL = "http://drive.example.com" },
		"device_name":      func(s *Settings) { s.DeviceName = "a\nb" },
	}
	for field, mutate := range cases {
		s := base
		mutate(&s)
		var fe *FieldError
		if err := s.Validate(); !errors.As(err, &fe) || fe.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	ok := base
	ok.ServerURL = "http://127.0.0.1:8080"
	if err := ok.Validate(); err != nil {
		t.Errorf("loopback http refused: %v", err)
	}
}

func TestPatch(t *testing.T) {
	base := Defaults(testEnv(t))
	next, err := Patch(base, []byte(`{"bandwidth":{"up_bps":5000000},"drive_letter":"x","notifications":{"held":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.Bandwidth.UpBPS != 5000000 || next.Bandwidth.DownBPS != 0 || next.DriveLetter != "X:" || next.Notifications.Held || !next.Notifications.Finished {
		t.Fatalf("patched = %+v", next)
	}
	changed := Changed(base, next)
	slices.Sort(changed)
	want := []string{"bandwidth.up_bps", "drive_letter", "notifications.held"}
	if !slices.Equal(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	var fe *FieldError
	if _, err := Patch(base, []byte(`{"nope":1}`)); !errors.As(err, &fe) || fe.Field != "nope" {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := Patch(base, []byte(`{"bandwidth":{"sideways":1}}`)); !errors.As(err, &fe) || fe.Field != "bandwidth.sideways" {
		t.Fatalf("unknown nested field: %v", err)
	}
	if _, err := Patch(base, []byte(`{"cache_cap_bytes":"big"}`)); err == nil {
		t.Fatal("wrong type accepted")
	}
	if _, err := Patch(base, []byte(`{"update_channel":"nightly"}`)); !errors.As(err, &fe) || fe.Field != "update_channel" {
		t.Fatalf("invalid value: %v", err)
	}
}
