// Package fakedrive is an in-memory drive for the client's tests: the
// upload routes (start with fingerprint resume, parts, complete, check),
// presigned storage, and the relay routes, with the drive's rules where
// the client depends on them. It is not the drive; the e2e test runs the
// real binary against the real one.
package fakedrive

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Drive is the fake. Lock-free fields are set before use.
type Drive struct {
	*httptest.Server
	SingleMax int64 // bigger files go multipart (default 2 KiB)
	PartSize  int64 // default 1 KiB
	// HashLater: versions get their sha256 only when HashAll is called
	// (the drive's worker hashes after publishing).
	HashLater bool
	// Fail, when it returns non-zero, answers that request with that status.
	Fail func(r *http.Request) int
	// Corrupt makes the drive's hash of every new version wrong.
	Corrupt bool

	mu       sync.Mutex
	seq      int
	folders  map[string]*folder // id → folder
	nodes    map[string]*file
	versions map[string]*version
	pending  map[string]*pending
	Puts     int // storage PUTs accepted
	Starts   int
	Beats    int
	Drops    []map[string]any // what /self returns
	Mode     string           // "personal" (default) or "shared"
}

type folder struct {
	id, parent, name string
	kids             map[string]string // lower(name) → id (folder or file)
}

type file struct {
	id, parent, name string
	versions         []*version
	trashed          bool
}

type version struct {
	id, node    string
	data        []byte
	sha         string
	fingerprint string
	person      string
}

type pending struct {
	id, parent, name, fingerprint, person, node string
	size                                        int64
	single                                      bool
	parts                                       map[int32][]byte
	done                                        *version
}

// Root is the drive's top folder id.
const Root = "root"

// New starts a fake drive; Close it when done.
func New() *Drive {
	d := &Drive{SingleMax: 2 << 10, PartSize: 1 << 10, folders: map[string]*folder{}, nodes: map[string]*file{},
		versions: map[string]*version{}, pending: map[string]*pending{}}
	d.folders[Root] = &folder{id: Root, name: "", kids: map[string]string{}}
	d.Server = httptest.NewServer(http.HandlerFunc(d.serve))
	return d
}

func (d *Drive) id(prefix string) string {
	d.seq++
	kinds := map[string]int{"up": 1, "node": 2, "dir": 3, "ver": 4}
	return fmt.Sprintf("%08d-0000-4000-8000-%012d", kinds[prefix], d.seq)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func refuse(w http.ResponseWriter, code int, kind, msg string) {
	reply(w, code, map[string]string{"error": kind, "message": msg})
}

func (d *Drive) serve(w http.ResponseWriter, r *http.Request) {
	if d.Fail != nil {
		if c := d.Fail(r); c != 0 {
			refuse(w, c, "injected", "injected failure")
			return
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	p := r.URL.Path
	if strings.HasPrefix(p, "/store/") {
		d.store(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer odt_EXAMPLE" {
		refuse(w, 401, "unauthorized", "sign in")
		return
	}
	switch {
	case r.Method == "POST" && p == "/api/v1/uploads":
		d.start(w, r)
	case r.Method == "POST" && p == "/api/v1/uploads/check":
		d.check(w, r)
	case r.Method == "POST" && strings.HasSuffix(p, "/parts"):
		d.parts(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/uploads/"), "/parts"))
	case r.Method == "POST" && strings.HasSuffix(p, "/complete"):
		d.complete(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/uploads/"), "/complete"))
	case r.Method == "GET" && strings.HasPrefix(p, "/api/relay/v1/uploads/"):
		d.relayUpload(w, strings.TrimPrefix(p, "/api/relay/v1/uploads/"))
	case r.Method == "POST" && p == "/api/relay/v1/folders":
		d.ensure(w, r)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/relay/v1/folders/"):
		id := strings.TrimPrefix(p, "/api/relay/v1/folders/")
		if f, ok := d.folders[id]; ok {
			reply(w, 200, map[string]string{"id": f.id, "name": f.name})
		} else {
			refuse(w, 404, "not_found", "not found")
		}
	case r.Method == "GET" && p == "/api/relay/v1/self":
		drops := d.Drops
		if drops == nil {
			drops = []map[string]any{}
		}
		mode := d.Mode
		if mode == "" {
			mode = "personal"
		}
		reply(w, 200, map[string]any{"id": "node-1", "name": "Test node", "mode": mode, "drops": drops,
			"person": map[string]string{"id": "person-1", "name": "Alex Example"}})
	case r.Method == "POST" && p == "/api/relay/v1/heartbeat":
		d.Beats++
		w.WriteHeader(204)
	default:
		refuse(w, 404, "not_found", "no such route "+p)
	}
}

func (d *Drive) lookup(parent, name string) string {
	f := d.folders[parent]
	if f == nil {
		return ""
	}
	return f.kids[strings.ToLower(name)]
}

func (d *Drive) start(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ParentID, Name, Mime, Conflict, Fingerprint, OnBehalfOf string
		Size                                                    int64
	}
	if err := json.NewDecoder(r.Body).Decode(&struct {
		P *string `json:"parent_id"`
		N *string `json:"name"`
		S *int64  `json:"size"`
		C *string `json:"conflict"`
		F *string `json:"fingerprint"`
		O *string `json:"on_behalf_of"`
	}{&in.ParentID, &in.Name, &in.Size, &in.Conflict, &in.Fingerprint, &in.OnBehalfOf}); err != nil {
		refuse(w, 422, "invalid", err.Error())
		return
	}
	if _, ok := d.folders[in.ParentID]; !ok {
		refuse(w, 404, "not_found", "no such folder")
		return
	}
	d.Starts++
	// Resume: the same file (parent, size, fingerprint) pending.
	if in.Fingerprint != "" {
		for _, p := range d.pending {
			if p.done == nil && p.parent == in.ParentID && p.size == in.Size && p.fingerprint == in.Fingerprint {
				reply(w, 201, d.ticket(r, p, true))
				return
			}
		}
	}
	name, node := in.Name, ""
	if id := d.lookup(in.ParentID, in.Name); id != "" {
		_, isFile := d.nodes[id]
		switch {
		case in.Conflict == "rename":
			for i := 2; ; i++ {
				ext := ""
				base := in.Name
				if j := strings.LastIndexByte(base, '.'); j > 0 {
					base, ext = base[:j], base[j:]
				}
				name = fmt.Sprintf("%s (%d)%s", base, i, ext)
				if d.lookup(in.ParentID, name) == "" {
					break
				}
			}
		case in.Conflict == "new_version" && isFile:
			node = id
		default:
			refuse(w, 409, "conflict", "an item with that name already exists")
			return
		}
	}
	p := &pending{id: d.id("up"), parent: in.ParentID, name: name, fingerprint: in.Fingerprint, person: in.OnBehalfOf,
		size: in.Size, node: node, parts: map[int32][]byte{}, single: in.Size <= d.SingleMax}
	d.pending[p.id] = p
	reply(w, 201, d.ticket(r, p, false))
}

func (d *Drive) storeURL(r *http.Request, id string, n int32) string {
	return "http://" + r.Host + "/store/" + id + "/" + strconv.Itoa(int(n))
}

func (d *Drive) ticket(r *http.Request, p *pending, resumed bool) map[string]any {
	t := map[string]any{"upload_id": p.id, "node_id": p.node, "version_id": p.id, "name": p.name, "resumed": resumed}
	if p.single {
		t["method"], t["url"], t["headers"] = "single", d.storeURL(r, p.id, 0), map[string]string{"If-None-Match": "*"}
		return t
	}
	count := int32((p.size + d.PartSize - 1) / d.PartSize)
	t["method"], t["part_size"], t["part_count"] = "multipart", d.PartSize, count
	var parts []map[string]any
	var have []map[string]any
	for n := int32(1); n <= count; n++ {
		if b, ok := p.parts[n]; ok {
			sum := md5.Sum(b)
			have = append(have, map[string]any{"number": n, "etag": `"` + hex.EncodeToString(sum[:]) + `"`, "size": len(b)})
		} else if len(parts) < 100 {
			parts = append(parts, map[string]any{"number": n, "url": d.storeURL(r, p.id, n)})
		}
	}
	t["parts"], t["uploaded"] = parts, have
	return t
}

func (d *Drive) parts(w http.ResponseWriter, r *http.Request, id string) {
	p := d.pending[id]
	if p == nil {
		refuse(w, 404, "not_found", "no such upload")
		return
	}
	var in struct {
		Numbers []int32 `json:"numbers"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	var out []map[string]any
	for _, n := range in.Numbers {
		out = append(out, map[string]any{"number": n, "url": d.storeURL(r, id, n)})
	}
	reply(w, 200, map[string]any{"parts": out})
}

func (d *Drive) store(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		refuse(w, 400, "auth_sent", "storage must never see the drive token")
		return
	}
	rest := strings.Split(strings.TrimPrefix(r.URL.Path, "/store/"), "/")
	p := d.pending[rest[0]]
	if p == nil || len(rest) != 2 {
		w.WriteHeader(404)
		return
	}
	n, _ := strconv.Atoi(rest[1])
	if p.single && r.Header.Get("If-None-Match") == "*" && p.parts[0] != nil {
		w.WriteHeader(412)
		return
	}
	b, _ := io.ReadAll(r.Body)
	p.parts[int32(n)] = b
	d.Puts++
	sum := md5.Sum(b)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(200)
}

func (d *Drive) complete(w http.ResponseWriter, r *http.Request, id string) {
	p := d.pending[id]
	if p == nil {
		refuse(w, 404, "not_found", "no such upload")
		return
	}
	if p.done != nil { // idempotent
		reply(w, 200, map[string]any{"id": p.done.node, "name": p.name, "parent_id": p.parent, "version_id": p.done.id, "size": p.size})
		return
	}
	var data []byte
	if p.single {
		data = p.parts[0]
	} else {
		keys := make([]int, 0, len(p.parts))
		for k := range p.parts {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		for _, k := range keys {
			data = append(data, p.parts[int32(k)]...)
		}
	}
	if int64(len(data)) != p.size {
		refuse(w, 409, "upload_incomplete", fmt.Sprintf("stored %d of %d bytes", len(data), p.size))
		return
	}
	f := d.nodes[p.node]
	if f == nil {
		if other := d.lookup(p.parent, p.name); other != "" {
			refuse(w, 409, "conflict", "an item with that name already exists")
			return
		}
		f = &file{id: d.id("node"), parent: p.parent, name: p.name}
		d.nodes[f.id] = f
		d.folders[p.parent].kids[strings.ToLower(p.name)] = f.id
	}
	v := &version{id: p.id, node: f.id, data: data, fingerprint: p.fingerprint, person: p.person}
	if !d.HashLater {
		v.sha = d.hash(data)
	}
	f.versions = append(f.versions, v)
	d.versions[v.id] = v
	p.done = v
	reply(w, 200, map[string]any{"id": f.id, "name": p.name, "parent_id": p.parent, "version_id": v.id, "size": p.size})
}

func (d *Drive) hash(b []byte) string {
	s := sha256.Sum256(b)
	if d.Corrupt {
		s[0] ^= 0xff
	}
	return hex.EncodeToString(s[:])
}

// HashAll is the drive's worker catching up.
func (d *Drive) HashAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range d.versions {
		if v.sha == "" {
			v.sha = d.hash(v.data)
		}
	}
}

func (d *Drive) check(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Files []struct {
			ParentID    string `json:"parent_id"`
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			Fingerprint string `json:"fingerprint"`
			SHA256      string `json:"sha256"`
		} `json:"files"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if len(in.Files) > 1000 {
		refuse(w, 422, "invalid", "at most 1000 files")
		return
	}
	var out []map[string]any
	for _, c := range in.Files {
		id := d.lookup(c.ParentID, c.Name)
		f := d.nodes[id]
		if id == "" {
			out = append(out, map[string]any{"state": "absent"})
			continue
		}
		if f == nil || f.trashed {
			out = append(out, map[string]any{"state": "different", "id": id})
			continue
		}
		v := f.versions[len(f.versions)-1]
		same := int64(len(v.data)) == c.Size
		o := map[string]any{"id": id, "same_size": same, "sha256": v.sha}
		if same && ((c.Fingerprint != "" && c.Fingerprint == v.fingerprint) || (c.SHA256 != "" && c.SHA256 == v.sha)) {
			o["state"] = "present"
		} else {
			o["state"] = "different"
		}
		out = append(out, o)
	}
	reply(w, 200, map[string]any{"items": out})
}

func (d *Drive) relayUpload(w http.ResponseWriter, id string) {
	v := d.versions[id]
	if v == nil {
		refuse(w, 404, "not_found", "not found")
		return
	}
	f := d.nodes[v.node]
	var sha any
	if v.sha != "" {
		sha = v.sha
	}
	reply(w, 200, map[string]any{"id": v.id, "status": "ready", "node_id": v.node, "size": len(v.data), "sha256": sha,
		"current": f.versions[len(f.versions)-1] == v, "trashed": f.trashed})
}

func (d *Drive) ensure(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ParentID string `json:"parent_id"`
		Path     string `json:"path"`
		FindOnly bool   `json:"find_only"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	cur := d.folders[in.ParentID]
	if cur == nil {
		refuse(w, 404, "not_found", "no such folder")
		return
	}
	for _, seg := range strings.Split(strings.Trim(in.Path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			refuse(w, 422, "invalid", "bad path")
			return
		}
		id := cur.kids[strings.ToLower(seg)]
		switch {
		case id != "" && d.folders[id] != nil:
			cur = d.folders[id]
		case id != "":
			refuse(w, 409, "conflict", fmt.Sprintf("%q is a file", seg))
			return
		case in.FindOnly:
			refuse(w, 404, "not_found", "doesn't exist yet")
			return
		default:
			f := &folder{id: d.id("dir"), parent: cur.id, name: seg, kids: map[string]string{}}
			d.folders[f.id] = f
			cur.kids[strings.ToLower(seg)] = f.id
			cur = f
		}
	}
	reply(w, 200, map[string]string{"id": cur.id, "name": cur.name})
}

// File is a stored file as tests see it.
type File struct {
	Path     string // "a/b.txt" from the root
	Versions int
	Data     []byte
	Person   string
}

// Files lists every file with its path.
func (d *Drive) Files() map[string]File {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]File{}
	for _, f := range d.nodes {
		path := f.name
		for p := d.folders[f.parent]; p != nil && p.id != Root; p = d.folders[p.parent] {
			path = p.name + "/" + path
		}
		v := f.versions[len(f.versions)-1]
		out[path] = File{Path: path, Versions: len(f.versions), Data: v.data, Person: v.person}
	}
	return out
}

// Folder makes (or finds) a folder path from the root and returns its id.
func (d *Drive) Folder(path string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.folders[Root]
	for _, seg := range strings.Split(path, "/") {
		id := cur.kids[strings.ToLower(seg)]
		if id == "" {
			f := &folder{id: d.id("dir"), parent: cur.id, name: seg, kids: map[string]string{}}
			d.folders[f.id] = f
			cur.kids[strings.ToLower(seg)] = f.id
			id = f.id
		}
		cur = d.folders[id]
	}
	return cur.id
}

// Put stores a file directly (someone else uploaded it through the web).
func (d *Drive) Put(parent, name string, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := &file{id: d.id("node"), parent: parent, name: name}
	v := &version{id: d.id("ver"), node: f.id, data: data, sha: d.hash(data)}
	f.versions = []*version{v}
	d.nodes[f.id] = f
	d.versions[v.id] = v
	d.folders[parent].kids[strings.ToLower(name)] = f.id
}

// Pending counts uploads started and never completed.
func (d *Drive) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, p := range d.pending {
		if p.done == nil {
			n++
		}
	}
	return n
}
