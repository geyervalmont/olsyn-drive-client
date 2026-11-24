package fakedrive

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// public
	mux.HandleFunc("POST /api/mount/v1/link", s.linkStart)
	mux.HandleFunc("POST /api/mount/v1/link/token", s.linkToken)
	mux.HandleFunc("GET /api/client/v1/releases/{file}", s.releaseManifest)
	mux.HandleFunc("PUT /s3/{upload}/{part}", s.storagePut)
	mux.HandleFunc("GET /link", s.linkPage)
	mux.HandleFunc("POST /link", s.linkDecide)
	// authenticated
	auth := func(h http.HandlerFunc) http.HandlerFunc { return s.authed(h) }
	mux.HandleFunc("GET /api/mount/v1/bootstrap", auth(s.bootstrap))
	mux.HandleFunc("POST /api/mount/v1/heartbeat", auth(s.heartbeatH))
	mux.HandleFunc("GET /api/mount/v1/nodes/{id}/content", auth(s.content))
	mux.HandleFunc("GET /api/v1/me", auth(s.me))
	mux.HandleFunc("DELETE /api/v1/devices/{id}", auth(s.unlinkDevice))
	mux.HandleFunc("GET /api/v1/nodes/{id}", auth(s.getNode))
	mux.HandleFunc("GET /api/v1/nodes/{id}/children", auth(s.children))
	mux.HandleFunc("POST /api/v1/nodes/{id}/folders", auth(s.createFolder))
	mux.HandleFunc("PATCH /api/v1/nodes/{id}", auth(s.patchNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", auth(s.trashNode))
	mux.HandleFunc("GET /api/v1/resolve", auth(s.resolve))
	mux.HandleFunc("GET /api/v1/changes", auth(s.changes))
	mux.HandleFunc("GET /api/v1/stream", auth(s.stream))
	mux.HandleFunc("POST /api/v1/uploads", auth(s.uploadStart))
	mux.HandleFunc("GET /api/v1/uploads", auth(s.uploadList))
	mux.HandleFunc("POST /api/v1/uploads/check", auth(s.uploadCheck))
	mux.HandleFunc("GET /api/v1/uploads/{id}", auth(s.uploadGet))
	mux.HandleFunc("POST /api/v1/uploads/{id}/parts", auth(s.uploadParts))
	mux.HandleFunc("POST /api/v1/uploads/{id}/complete", auth(s.uploadComplete))
	mux.HandleFunc("DELETE /api/v1/uploads/{id}", auth(s.uploadAbort))
	return s.faulty(mux)
}

// jitter draws random delays from a seeded generator (see SetJitter).
type jitter struct {
	mu  sync.Mutex
	max time.Duration
	rng *rand.Rand
}

func newJitter(seed uint64, max time.Duration) *jitter {
	return &jitter{max: max, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (j *jitter) next() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return time.Duration(j.rng.Int64N(int64(j.max)))
}

// lateAnswer holds an answer back for a moment after the handler has made
// its change, as a slow network would.
type lateAnswer struct {
	http.ResponseWriter
	delay time.Duration
	once  sync.Once
}

func (l *lateAnswer) wait() { l.once.Do(func() { time.Sleep(l.delay) }) }

func (l *lateAnswer) WriteHeader(code int) {
	l.wait()
	l.ResponseWriter.WriteHeader(code)
}

func (l *lateAnswer) Write(b []byte) (int, error) {
	l.wait()
	return l.ResponseWriter.Write(b)
}

// faulty applies intercepts, the offline switch, the deploy window and latency.
func (s *Server) faulty(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		f := s.faults
		icpt := s.intercept
		s.mu.Unlock()
		if icpt != nil && icpt(w, r) {
			return
		}
		if f.offline {
			hangUp(w)
			return
		}
		if f.latency > 0 {
			select {
			case <-time.After(f.latency):
			case <-r.Context().Done():
				return
			}
		}
		if f.jitter != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			select {
			case <-time.After(f.jitter.next()):
			case <-r.Context().Done():
				return
			}
			w = &lateAnswer{ResponseWriter: w, delay: f.jitter.next()}
		}
		storage := strings.HasPrefix(r.URL.Path, "/s3/")
		if !storage && time.Now().Before(f.unavailableUntil) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "upstream unavailable\n")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hangUp drops the connection without an answer.
func hangUp(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		valid := ok && s.tokens[tok]
		s.mu.Unlock()
		if !valid {
			fail(w, http.StatusUnauthorized, driveapi.CodeUnauthenticated, "sign in again", nil)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"error": code, "message": msg}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, body)
}

func refusalStatus(code string) int {
	switch code {
	case driveapi.CodeSealed, driveapi.CodeForbidden:
		return http.StatusForbidden
	case driveapi.CodeQuotaExceeded:
		return http.StatusRequestEntityTooLarge
	case driveapi.CodeRetentionPeriod, driveapi.CodeArchived:
		return http.StatusConflict
	}
	return http.StatusLocked
}

// refuseLocked answers a write refusal for anything inside id; true if it did.
func (s *Server) refuseLocked(w http.ResponseWriter, id string) bool {
	code := s.refusalLocked(id)
	if code == "" {
		return false
	}
	extra := map[string]any{}
	switch code {
	case driveapi.CodeManaged:
		extra["managed_by"] = "Test Integration"
	case driveapi.CodeSealed:
		extra["sealed_folder"] = map[string]string{"id": id, "name": "Sealed"}
	}
	fail(w, refusalStatus(code), code, "refused by the fake drive", extra)
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "bad body: "+err.Error(), nil)
		return false
	}
	return true
}

func (s *Server) count(name string) {
	s.mu.Lock()
	s.counts[name]++
	s.mu.Unlock()
}

// ---- linking, bootstrap, heartbeat, releases ----

func (s *Server) linkStart(w http.ResponseWriter, r *http.Request) {
	var req driveapi.LinkRequest
	if !decode(w, r, &req) {
		return
	}
	dc, uc := "odl_"+randToken(), userCode()
	s.mu.Lock()
	s.links[dc] = &link{userCode: uc, expires: time.Now().Add(10 * time.Minute), approved: s.opts.AutoApprove}
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, driveapi.LinkStart{DeviceCode: dc, UserCode: uc,
		VerificationURL: s.srv.URL + "/link?code=" + uc, Interval: 1, ExpiresIn: 600})
}

// ApproveLinks approves every pending link.
func (s *Server) ApproveLinks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		l.approved = true
	}
}

// DenyLinks denies every pending link.
func (s *Server) DenyLinks() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		l.denied = true
	}
}

func (s *Server) linkToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.links[req.DeviceCode]
	switch {
	case l == nil:
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such code", nil)
	case l.claimed || time.Now().After(l.expires):
		fail(w, http.StatusGone, "expired", "expired", nil)
	case l.denied:
		fail(w, http.StatusForbidden, "denied", "denied", nil)
	case !l.approved || l.polls < s.opts.LinkPendingPolls:
		l.polls++
		fail(w, http.StatusPreconditionRequired, driveapi.CodeAuthPending, "waiting for approval", nil)
	default:
		l.claimed = true
		tok, dev := "odt_"+randToken(), newID()
		s.tokens[tok] = true
		s.devices[dev] = tok
		writeJSON(w, http.StatusOK, driveapi.LinkResult{Token: tok, DeviceID: dev, User: driveapi.User{ID: "u-test", Name: "Test Person"}})
	}
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	b := map[string]any{
		"contract": "olsyn-drive/1", "user": driveapi.User{ID: "u-test", Name: "Test Person"}, "read_only": false,
		"web_url":      s.srv.URL,
		"capabilities": map[string]bool{"write": true, "range_reads": true, "conditional_saves": true, "generated": false},
		"limits":       map[string]int64{"max_range_bytes": 64 << 20, "max_file_bytes": 5 << 40},
		"refresh":      map[string]int{"manifest_seconds": 30, "heartbeat_seconds": 30, "access_recheck_seconds": 15},
		"endpoints":    map[string]string{"content": s.srv.URL + "/api/mount/v1/nodes/{id}/content"},
	}
	writeJSON(w, http.StatusOK, b)
}

// unlinkDevice revokes a linked device and its token.
func (s *Server) unlinkDevice(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	tok, ok := s.devices[r.PathValue("id")]
	if ok {
		delete(s.tokens, tok)
		delete(s.devices, r.PathValue("id"))
	}
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such device", nil)
		return
	}
	s.DropStreams()
	w.WriteHeader(http.StatusNoContent)
}

// RevokeDevices revokes every linked device's token (as an admin removing
// them in the web app would).
func (s *Server) RevokeDevices() {
	s.mu.Lock()
	for id, tok := range s.devices {
		delete(s.tokens, tok)
		delete(s.devices, id)
	}
	s.mu.Unlock()
	s.DropStreams()
}

func (s *Server) heartbeatH(w http.ResponseWriter, r *http.Request) {
	var hb driveapi.Heartbeat
	if !decode(w, r, &hb) {
		return
	}
	s.mu.Lock()
	s.heartbeat = &hb
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) releaseManifest(w http.ResponseWriter, r *http.Request) {
	ch, ok := strings.CutSuffix(r.PathValue("file"), ".json")
	s.mu.Lock()
	body := s.release[ch]
	s.mu.Unlock()
	if !ok || body == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no release", nil)
		return
	}
	etag := etagOf(body)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// ReleaseBody builds a release answer from manifest bytes and a signature.
func ReleaseBody(channel string, manifest []byte, keyID string, sig []byte) []byte {
	b, _ := json.Marshal(map[string]any{"channel": channel, "manifest": base64.StdEncoding.EncodeToString(manifest),
		"signature": map[string]string{"alg": "ed25519", "key_id": keyID, "sig": base64.StdEncoding.EncodeToString(sig)}})
	return b
}

// ---- nodes ----

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": driveapi.User{ID: "u-test", Name: "Test Person"}, "root_id": s.rootID})
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[r.PathValue("id")]
	if n == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": s.toAPILocked(n), "path": []any{}})
}

func (s *Server) children(w http.ResponseWriter, r *http.Request) {
	s.count("children")
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[r.PathValue("id")]
	if n == nil || n.kind != driveapi.KindFolder || !s.liveLocked(n) {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	kids := s.childrenLocked(n.id)
	if start > len(kids) {
		start = len(kids)
	}
	end := min(start+limit, len(kids))
	items := make([]driveapi.Node, 0, end-start)
	for _, c := range kids[start:end] {
		items = append(items, s.toAPILocked(c))
	}
	var next any
	if end < len(kids) {
		next = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	s.count("folder.create")
	var req struct {
		Name       string         `json:"name"`
		Conflict   string         `json:"conflict"`
		ModifiedAt *driveapi.Time `json:"modified_at"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[r.PathValue("id")]
	if p == nil || p.kind != driveapi.KindFolder || !s.liveLocked(p) {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	if s.refuseLocked(w, p.id) {
		return
	}
	if !validName(req.Name) {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalidName, "bad name", nil)
		return
	}
	name := req.Name
	if s.childByNameLocked(p.id, name) != nil {
		if req.Conflict != driveapi.ConflictRename {
			fail(w, http.StatusConflict, driveapi.CodeNameConflict, "name taken", nil)
			return
		}
		name = freeName(s, p.id, name)
	}
	n := s.addNodeLocked(p.id, driveapi.KindFolder, name)
	if req.ModifiedAt != nil {
		t := req.ModifiedAt.Time
		n.modifiedAt = &t
	}
	writeJSON(w, http.StatusCreated, s.toAPILocked(n))
}

func (s *Server) patchNode(w http.ResponseWriter, r *http.Request) {
	s.count("patch")
	var req map[string]json.RawMessage
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[r.PathValue("id")]
	if n == nil || !s.liveLocked(n) {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	if s.refuseLocked(w, n.id) {
		return
	}
	name, parent := n.name, n.parentID
	if raw, ok := req["name"]; ok {
		_ = json.Unmarshal(raw, &name)
		if !validName(name) {
			fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalidName, "bad name", nil)
			return
		}
	}
	if raw, ok := req["parent_id"]; ok {
		_ = json.Unmarshal(raw, &parent)
		p := s.nodes[parent]
		if p == nil || p.kind != driveapi.KindFolder || !s.liveLocked(p) {
			fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such parent", nil)
			return
		}
		if s.refuseLocked(w, parent) {
			return
		}
	}
	if other := s.childByNameLocked(parent, name); other != nil && other != n {
		fail(w, http.StatusConflict, driveapi.CodeNameConflict, "name taken", nil)
		return
	}
	n.name, n.parentID = name, parent
	if raw, ok := req["modified_at"]; ok {
		var t driveapi.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalidModifiedAt, err.Error(), nil)
			return
		}
		if t.IsZero() {
			n.modifiedAt = nil
		} else {
			tt := t.Time
			n.modifiedAt = &tt
		}
	}
	n.updated = time.Now().UTC()
	s.emitLocked(n.id, "")
	writeJSON(w, http.StatusOK, s.toAPILocked(n))
}

func (s *Server) trashNode(w http.ResponseWriter, r *http.Request) {
	s.count("trash")
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[r.PathValue("id")]
	if n == nil || !s.liveLocked(n) || n.id == s.rootID {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	if s.refuseLocked(w, n.id) {
		return
	}
	t := time.Now().UTC()
	n.trashed = &t
	s.emitLocked(n.id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n *node
	if uri := r.URL.Query().Get("uri"); uri != "" {
		_, id, _ := strings.Cut(uri, "/n/")
		if cand := s.nodes[id]; cand != nil && s.liveLocked(cand) {
			n = cand
		}
	} else {
		n = s.resolveLocked(r.URL.Query().Get("path"))
	}
	if n == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.toAPILocked(n))
}

// ---- content ----

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	s.count("download")
	s.mu.Lock()
	n := s.nodes[r.PathValue("id")]
	if n == nil || n.kind != driveapi.KindFile || !s.liveLocked(n) {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "not found", nil)
		return
	}
	if n.gen != nil {
		s.generatedContent(w, r, n)
		return
	}
	v := n.current()
	if want := r.URL.Query().Get("version"); want != "" {
		v = nil
		for _, cand := range n.versions {
			if cand.id == want {
				v = cand
			}
		}
	}
	drop := int64(-1)
	if s.faults.dropDownloads > 0 {
		s.faults.dropDownloads--
		drop = s.faults.dropDownloadAfter
	}
	s.mu.Unlock()
	if v == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such version", nil)
		return
	}
	serveRange(w, r, v.content, driveapi.VersionETag(v.id), drop)
}

// generatedContent serves a generated file: no versions, the result's ETag,
// built by the first GET. Called with s.mu held; it unlocks.
func (s *Server) generatedContent(w http.ResponseWriter, r *http.Request, n *node) {
	g := n.gen
	if r.URL.Query().Get("version") != "" {
		s.mu.Unlock()
		fail(w, http.StatusUnprocessableEntity, "invalid", "generated files have no versions", nil)
		return
	}
	if !g.built {
		if r.Method == http.MethodHead {
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		if g.busy > 0 {
			g.busy--
			s.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			fail(w, http.StatusServiceUnavailable, "building", "being built", nil)
			return
		}
		g.built = true
	}
	data, etag := g.data, g.etag()
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "private, no-cache")
	serveRange(w, r, memBlob(data), etag, -1)
}

// serveRange answers one byte range of content (or all of it).
func serveRange(w http.ResponseWriter, r *http.Request, content blob, etag string, drop int64) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	total := content.size()
	start, end := int64(0), total-1
	status := http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		spec, ok := strings.CutPrefix(rg, "bytes=")
		if !ok || strings.Contains(spec, ",") {
			fail(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "one range", nil)
			return
		}
		a, b, _ := strings.Cut(spec, "-")
		start, _ = strconv.ParseInt(a, 10, 64)
		if b != "" {
			end, _ = strconv.ParseInt(b, 10, 64)
		}
		if end >= total {
			end = total - 1
		}
		if start >= total || start > end {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			fail(w, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "out of range", nil)
			return
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	}
	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if drop >= 0 && drop < length {
		_ = content.writeRange(w, start, drop)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	_ = content.writeRange(w, start, length)
}

// ---- changes and stream ----

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	s.count("changes")
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	// 60 a minute per credential, as the real feed
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.rateLimitedByLocked(w, "changes", "changes|"+tok, s.opts.ChangesRate, s.opts.ChangesRetryAfter) {
		return
	}
	if s.faults.reset != "" {
		reason := s.faults.reset
		s.faults.reset = ""
		writeJSON(w, http.StatusOK, map[string]any{"changes": []any{}, "next": s.seq, "has_more": false, "reset": map[string]string{"reason": reason}})
		return
	}
	if !q.Has("since") {
		writeJSON(w, http.StatusOK, map[string]any{"changes": []any{}, "next": s.seq, "has_more": false, "reset": map[string]string{"reason": driveapi.ResetNoCursor}})
		return
	}
	since, err := strconv.ParseInt(q.Get("since"), 10, 64)
	if err != nil || since > s.seq || since < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"changes": []any{}, "next": s.seq, "has_more": false, "reset": map[string]string{"reason": driveapi.ResetCursorInvalid}})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	// events are in seq order with seq == index+1
	from := int(since)
	to := min(from+limit, len(s.events))
	page := s.events[from:to]
	last := map[string]int64{}
	for _, e := range page {
		last[e.nodeID+"|"+e.op] = e.seq
	}
	var out []driveapi.Change
	for _, e := range page {
		if last[e.nodeID+"|"+e.op] != e.seq {
			continue
		}
		if e.op == driveapi.OpResync {
			out = append(out, driveapi.Change{Seq: e.seq, Op: driveapi.OpResync, FolderID: e.nodeID})
			continue
		}
		n := s.nodes[e.nodeID]
		switch {
		case n == nil:
			out = append(out, driveapi.Change{Seq: e.seq, Op: driveapi.OpRemoved, ID: e.nodeID})
		case !s.liveLocked(n):
			api := s.toAPILocked(n)
			out = append(out, driveapi.Change{Seq: e.seq, Op: driveapi.OpTrashed, Node: &api})
		default:
			api := s.toAPILocked(n)
			out = append(out, driveapi.Change{Seq: e.seq, Op: driveapi.OpUpsert, Node: &api})
		}
	}
	if out == nil {
		out = []driveapi.Change{}
	}
	next := since
	if len(page) > 0 {
		next = page[len(page)-1].seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out, "next": next, "has_more": to < len(s.events), "reset": nil})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, driveapi.CodeInternal, "no flusher", nil)
		return
	}
	ch := make(chan string, 64)
	s.mu.Lock()
	s.streams[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.streams[ch]; ok {
			delete(s.streams, ch)
			close(ch)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = io.WriteString(w, "event: change\ndata: "+msg+"\n\n")
			fl.Flush()
		}
	}
}
