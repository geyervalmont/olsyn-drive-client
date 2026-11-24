package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"olsyn-drive/internal/driveapi"
)

func TestSignInStoresPrivately(t *testing.T) {
	defaultPoll = 10 * time.Millisecond
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/relay/v1/enroll":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"device_code": "odl_x", "user_code": "ABCD-EFGH", "verification_url": "https://drive.example.com/link/ABCD-EFGH", "interval": 0, "expires_in": 60})
		case "/api/relay/v1/enroll/token":
			if polls.Add(1) < 2 {
				w.WriteHeader(428)
				w.Write([]byte(`{"error":"pending"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"token": "odt_EXAMPLE-not-a-real-token", "node_id": "n1", "node_name": "Alex on EXAMPLE-PC", "person": map[string]string{"id": "p1", "name": "Alex Example"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	api, err := driveapi.New(srv.URL, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "home")
	var shown string
	l, err := SignIn(context.Background(), api, Store{dir}, "EXAMPLE-PC", "0.1.0", func(code, link string) { shown = code + " " + link })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown, "ABCD-EFGH") || l.NodeName != "Alex on EXAMPLE-PC" {
		t.Fatalf("shown %q login %+v", shown, l)
	}
	got, tok, err := Store{dir}.Load()
	if err != nil || tok != "odt_EXAMPLE-not-a-real-token" || got.PersonID != "p1" {
		t.Fatalf("load: %v %q %+v", err, tok, got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "login.json"))
	if strings.Contains(string(b), "odt_") {
		t.Fatal("token written into login.json")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(tokenPath(dir))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("token mode %v", st.Mode().Perm())
		}
		os.Chmod(tokenPath(dir), 0o644)
		if _, _, err := (Store{dir}).Load(); err == nil {
			t.Fatal("loaded a token others can read")
		}
	}
	if err := (Store{dir}).Forget(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Store{dir}).Load(); err == nil {
		t.Fatal("still signed in after Forget")
	}
}
