// Package status serves the running client's state on loopback only:
// a small page at /, JSON at /status (what `olsyn-drive status` reads)
// and Prometheus text at /metrics. It holds no secrets and accepts no
// writes.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// DefaultAddr is where the client listens unless configured otherwise.
const DefaultAddr = "127.0.0.1:47111"

// Snapshot is the state shown.
type Snapshot struct {
	Mode       string    `json:"mode"` // relay
	Node       string    `json:"node"`
	Person     string    `json:"person,omitempty"`
	Drive      string    `json:"drive"`
	Version    string    `json:"version"`
	Spool      string    `json:"spool"`
	Started    time.Time `json:"started"`
	Waiting    int64     `json:"waiting"`
	Queued     int64     `json:"queued"`
	Held       int64     `json:"held"`
	Failed     int64     `json:"failed"`
	Unmapped   int64     `json:"unmapped"`
	SpoolBytes int64     `json:"spool_bytes"`
	FreeBytes  int64     `json:"free_bytes"`
	Full       bool      `json:"full"`
	SentFiles  int64     `json:"sent_files"`
	SentBytes  int64     `json:"sent_bytes"`
	LastScan   time.Time `json:"last_scan"`
	LastError  string    `json:"last_error,omitempty"`
	Drops      []Drop    `json:"drops"`
	Problems   []Problem `json:"problems,omitempty"`
}

type Drop struct {
	Path   string `json:"path"`
	Folder string `json:"folder"`
	Policy string `json:"on_conflict"`
}

type Problem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Serve listens on addr (loopback only) until ctx ends.
func Serve(ctx context.Context, addr string, snap func() Snapshot) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("the status page only listens on loopback, not %q", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap())
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		Metrics(w, snap())
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		page.Execute(w, snap())
	})
	srv := &http.Server{Addr: addr, Handler: hostCheck(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// hostCheck refuses requests whose Host isn't loopback (DNS rebinding:
// a web page making the browser talk to this port under another name).
func hostCheck(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden", 403)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Metrics writes Prometheus text.
func Metrics(w interface{ Write([]byte) (int, error) }, s Snapshot) {
	b2i := map[bool]int{false: 0, true: 1}
	lines := []struct {
		name, help, kind string
		v                any
	}{
		{"olsyn_drive_files", "Files in the spool by state.", "gauge", nil},
		{"olsyn_drive_spool_bytes", "Bytes in the spool.", "gauge", s.SpoolBytes},
		{"olsyn_drive_free_bytes", "Free bytes on the spool's disk.", "gauge", s.FreeBytes},
		{"olsyn_drive_full", "1 while the spool refuses new files.", "gauge", b2i[s.Full]},
		{"olsyn_drive_sent_files_total", "Files delivered and verified.", "counter", s.SentFiles},
		{"olsyn_drive_sent_bytes_total", "Bytes delivered and verified.", "counter", s.SentBytes},
		{"olsyn_drive_last_scan_timestamp_seconds", "When the spool was last scanned.", "gauge", s.LastScan.Unix()},
	}
	for _, l := range lines {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", l.name, l.help, l.name, l.kind)
		if l.v == nil {
			states := map[string]int64{"waiting": s.Waiting, "queued": s.Queued, "held": s.Held, "failed": s.Failed, "unmapped": s.Unmapped}
			keys := make([]string, 0, len(states))
			for k := range states {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "%s{state=%q} %d\n", l.name, k, states[k])
			}
			continue
		}
		fmt.Fprintf(w, "%s %v\n", l.name, l.v)
	}
}

// Bytes formats a size for people.
func Bytes(n int64) string {
	switch {
	case n < 0:
		return "unknown"
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

var page = template.Must(template.New("p").Funcs(template.FuncMap{"bytes": Bytes}).Parse(`<!doctype html>
<meta charset="utf-8"><meta http-equiv="refresh" content="5"><title>Olsyn Drive</title>
<style>body{font:14px system-ui,sans-serif;margin:24px;max-width:860px;color:#222;background:#fff}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#161616}td,th{border-color:#333!important}}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:4px 8px;border-bottom:1px solid #ddd}
.bad{color:#b00020}</style>
<h1>Olsyn Drive — {{.Node}}</h1>
<p>{{if .Person}}Signed in as {{.Person}} · {{end}}{{.Drive}} · version {{.Version}}</p>
{{if .Full}}<p class="bad">The spool is full: new files are refused until it drains.</p>{{end}}
{{if .LastError}}<p class="bad">Last error: {{.LastError}}</p>{{end}}
<table><tr><th>Arriving</th><th>Queued</th><th>Held</th><th>Failed</th><th>Outside drop folders</th><th>Delivered</th></tr>
<tr><td>{{.Waiting}}</td><td>{{.Queued}}</td><td>{{.Held}}</td><td>{{.Failed}}</td><td>{{.Unmapped}}</td><td>{{.SentFiles}} ({{bytes .SentBytes}})</td></tr></table>
<p>Spool: {{.Spool}} · {{bytes .SpoolBytes}} waiting · {{bytes .FreeBytes}} free</p>
<h2>Drop folders</h2><table><tr><th>Folder here</th><th>Goes to</th><th>If the name is taken</th></tr>
{{range .Drops}}<tr><td>{{.Path}}</td><td>{{.Folder}}</td><td>{{.Policy}}</td></tr>{{else}}<tr><td colspan=3>None yet: make one in the drive (“Send through a relay…”).</td></tr>{{end}}</table>
{{if .Problems}}<h2>Needs a person</h2><table><tr><th>File</th><th>Why</th></tr>
{{range .Problems}}<tr><td>{{.Path}}</td><td>{{.Reason}}</td></tr>{{end}}</table>{{end}}
`))
