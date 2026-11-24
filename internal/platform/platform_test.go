package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCredentialTarget(t *testing.T) {
	got, err := CredentialTarget("https://Drive.Example.com:8443/base")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Olsyn Drive/drive.example.com:8443" {
		t.Fatalf("target = %q", got)
	}
	if _, err := CredentialTarget("not a url"); err == nil {
		t.Fatal("expected an error for a url without host")
	}
}

func TestMemoryCredentialStore(t *testing.T) {
	s := NewMemoryCredentialStore()
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	secret := []byte("synthetic-secret")
	if err := s.Set("a", secret); err != nil {
		t.Fatal(err)
	}
	secret[0] = 'X' // the store must keep its own copy
	got, err := s.Get("a")
	if err != nil || string(got) != "synthetic-secret" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPlatformName(t *testing.T) {
	cases := map[[2]string]string{
		{"windows", "amd64"}: "windows-x64",
		{"windows", "arm64"}: "windows-arm64",
		{"darwin", "arm64"}:  "macos-arm64",
		{"linux", "amd64"}:   "linux-x64",
	}
	for in, want := range cases {
		if got := PlatformName(in[0], in[1]); got != want {
			t.Errorf("PlatformName(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateOpenURL(t *testing.T) {
	for _, ok := range []string{"https://drive.olsyn.com/link?code=ABCD", "http://127.0.0.1:8080/"} {
		if err := ValidateOpenURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "ms-settings:", "https://"} {
		if err := ValidateOpenURL(bad); err == nil {
			t.Errorf("%s: expected refusal", bad)
		}
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "odp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestUnixListenDial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	p := Under(shortTempDir(t))
	ep, err := Endpoint(p)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(ep)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(ep)); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode().Perm())
	}
	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Dial(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read %q %v", buf, err)
	}
	c.Close()

	if _, err := Listen(ep); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second listen: %v", err)
	}
	l.Close()

	// A stale socket file (no listener) is replaced.
	if err := os.WriteFile(ep, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l2, err := Listen(ep)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	l2.Close()
}

func TestAcquireInstance(t *testing.T) {
	p := Under(shortTempDir(t))
	release, err := AcquireInstance(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireInstance(p); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire: %v", err)
	}
	release()
	release2, err := AcquireInstance(p)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

func TestFreeDiskBytes(t *testing.T) {
	n, err := FreeDiskBytes(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no free space reported")
	}
}
