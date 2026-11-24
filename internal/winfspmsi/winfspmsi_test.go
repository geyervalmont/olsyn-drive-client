package winfspmsi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The pinned download is the WinFsp project's own release asset.
func TestPinnedURL(t *testing.T) {
	want := "https://github.com/winfsp/winfsp/releases/download/v2.2/winfsp-2.2.26323.msi"
	if URL != want {
		t.Fatalf("URL = %s, want %s", URL, want)
	}
	if len(SHA256) != 64 || Size <= 0 {
		t.Fatal("pins are missing")
	}
}

func fakeInstaller() ([]byte, string) {
	data := bytes.Repeat([]byte("synthetic installer bytes "), 20000) // ~520 KB
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

func TestFetchChecksSizeAndHash(t *testing.T) {
	data, sum := fakeInstaller()
	serve := data
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(serve)))
		w.Write(serve)
	}))
	defer srv.Close()
	f := Fetcher{HTTP: srv.Client(), URL: srv.URL + "/winfsp.msi", Size: int64(len(data)), SHA256: sum}
	dir := t.TempDir()

	var reports []int64
	dst := filepath.Join(dir, "ok.msi")
	if err := f.Fetch(context.Background(), dst, func(done, total int64) {
		if total != int64(len(data)) {
			t.Errorf("total %d", total)
		}
		reports = append(reports, done)
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("downloaded file differs")
	}
	if len(reports) < 2 || reports[len(reports)-1] != int64(len(data)) {
		t.Fatalf("progress reports %v", reports)
	}

	// Same size, different bytes (a tampered or substituted file).
	serve = bytes.Repeat([]byte("x"), len(data))
	dst = filepath.Join(dir, "tampered.msi")
	err := f.Fetch(context.Background(), dst, nil)
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("a file that failed its check was left behind")
	}

	// A different size (a proxy's block page, say).
	serve = []byte("<html>blocked</html>")
	if err := f.Fetch(context.Background(), filepath.Join(dir, "short.msi"), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("short: %v", err)
	}
}

func TestFetchUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	f := Fetcher{HTTP: srv.Client(), URL: srv.URL, Size: 10, SHA256: strings.Repeat("0", 64)}
	if err := f.Fetch(context.Background(), filepath.Join(t.TempDir(), "a.msi"), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("403: %v", err)
	}
	srv.Close()
	// Offline: nothing listens any more.
	if err := f.Fetch(context.Background(), filepath.Join(t.TempDir(), "b.msi"), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("offline: %v", err)
	}
}
