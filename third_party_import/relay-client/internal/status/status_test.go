package status

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostCheckAndMetrics(t *testing.T) {
	h := hostCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for host, want := range map[string]int{"127.0.0.1:47111": 200, "localhost:47111": 200, "[::1]:47111": 200, "evil.example.com": 403, "evil.example.com:47111": 403} {
		r := httptest.NewRequest("GET", "/status", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %s: %d, want %d", host, w.Code, want)
		}
	}
	var b strings.Builder
	Metrics(&b, Snapshot{Held: 2, SpoolBytes: 10, Full: true})
	for _, want := range []string{`olsyn_drive_files{state="held"} 2`, "olsyn_drive_spool_bytes 10", "olsyn_drive_full 1"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if err := Serve(t.Context(), "0.0.0.0:0", func() Snapshot { return Snapshot{} }); err == nil {
		t.Error("listened on all interfaces")
	}
}
