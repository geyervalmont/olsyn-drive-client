// Package fakedrive is an in-process fake of the Olsyn drive's HTTP API for
// tests: the routes the desktop client uses, held in memory, with fault
// injection (latency, dropped connections, a "deploy" of 503s, revoked
// tokens, refused folders, quotas, expired part URLs, forced resets).
//
// It follows docs/api.md and the client route shapes closely enough to test
// the client's behaviour, not the server's every rule.
package fakedrive

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// Options configure a fake drive.
type Options struct {
	// PartSize for multipart uploads (the real drive uses ≥ 16 MiB). Default 1 MiB.
	PartSize int64
	// SingleMax is the largest single-PUT upload (real: 32 MiB). Default 2 MiB.
	SingleMax int64
	// LinkPendingPolls is how many polls answer authorization_pending before
	// an approved link hands over its token. Default 1.
	LinkPendingPolls int
	// Addr is the address to listen on ("127.0.0.1:8787"); default a free
	// loopback port.
	Addr string
	// DataDir keeps uploaded content in files under it instead of memory.
	DataDir string
	// AutoApprove approves every sign-in link as soon as it starts; otherwise
	// the link page (GET /link) has an Approve button, or call ApproveLinks.
	AutoApprove bool
	// VerifyMax is the largest upload whose SHA-256 the drive checks at
	// completion (real: 64 MiB); larger ones are "claimed" until the
	// checksum pass (RunChecksumPass). Default 64 MiB.
	VerifyMax int64
	// ChecksumPassDelay, when set, runs the checksum pass that long after
	// each claimed upload, as the real drive's worker does.
	ChecksumPassDelay time.Duration
}

// Server is a running fake drive.
type Server struct {
	srv  *httptest.Server
	opts Options

	mu        sync.Mutex
	rootID    string
	nodes     map[string]*node
	events    []event
	seq       int64
	uploads   map[string]*upload
	completed map[string]string // upload id -> node id
	tokens    map[string]bool
	devices   map[string]string // device id -> its token
	links     map[string]*link  // by device code
	sigs      map[string]time.Time
	refuse    map[string]string // folder id -> error code
	quota     map[string]int64  // folder id -> bytes
	release   map[string][]byte // channel -> JSON body
	streams   map[chan string]struct{}
	counts    map[string]int
	heartbeat *driveapi.Heartbeat
	faults    faults
	intercept func(w http.ResponseWriter, r *http.Request) bool
}

type faults struct {
	latency           time.Duration
	unavailableUntil  time.Time
	offline           bool
	dropDownloads     int
	dropDownloadAfter int64
	dropPuts          int
	dropPutAfter      int64
	reset             string
	corruptUploads    int // completes whose stored bytes "differ" from what was sent
}

type node struct {
	id         string
	parentID   string
	kind       string
	name       string
	created    time.Time
	updated    time.Time
	modifiedAt *time.Time
	trashed    *time.Time
	versions   []*version
}

type version struct {
	id          string
	number      int
	content     blob
	sha256      string
	fingerprint string
	created     time.Time
	modifiedAt  *time.Time
	// checksum check: state "" (no claim), verified, claimed or mismatch;
	// claimed is the client's SHA-256 while the pass is to come; corrupt
	// makes the pass find other bytes (a storage fault).
	checksumState string
	claimed       string
	corrupt       bool
}

type event struct {
	seq    int64
	nodeID string
	op     string // "" (derive from state) or "resync"
}

type upload struct {
	id          string
	nodeID      string
	versionID   string
	parentID    string
	name        string
	size        int64
	mime        string
	method      string
	partSize    int64
	partCount   int
	conflict    string
	base        string
	fingerprint string
	modifiedAt  *time.Time
	parts       map[int]part
	created     time.Time
	expires     time.Time
}

type part struct {
	seg  segment
	etag string
}

type link struct {
	userCode string
	approved bool
	denied   bool
	claimed  bool
	polls    int
	expires  time.Time
}

// New starts a fake drive with an empty root folder and one linked token
// (Token()).
func New(opts Options) *Server {
	if opts.PartSize <= 0 {
		opts.PartSize = 1 << 20
	}
	if opts.SingleMax <= 0 {
		opts.SingleMax = 2 << 20
	}
	if opts.LinkPendingPolls <= 0 {
		opts.LinkPendingPolls = 1
	}
	if opts.VerifyMax <= 0 {
		opts.VerifyMax = 64 << 20
	}
	s := &Server{
		opts:      opts,
		nodes:     map[string]*node{},
		uploads:   map[string]*upload{},
		completed: map[string]string{},
		tokens:    map[string]bool{testToken: true},
		devices:   map[string]string{},
		links:     map[string]*link{},
		sigs:      map[string]time.Time{},
		refuse:    map[string]string{},
		quota:     map[string]int64{},
		release:   map[string][]byte{},
		streams:   map[chan string]struct{}{},
		counts:    map[string]int{},
	}
	now := time.Now().UTC()
	s.rootID = newID()
	s.nodes[s.rootID] = &node{id: s.rootID, kind: driveapi.KindFolder, name: "Drive", created: now, updated: now}
	s.srv = httptest.NewUnstartedServer(s.routes())
	if opts.Addr != "" {
		l, err := net.Listen("tcp", opts.Addr)
		if err != nil {
			panic(fmt.Sprintf("fakedrive: listen on %s: %v", opts.Addr, err))
		}
		_ = s.srv.Listener.Close()
		s.srv.Listener = l
	}
	s.srv.Start()
	return s
}

const testToken = "odt_fake_token"

// managedBy is the "managed" field of files in a folder refused with
// "managed": synthetic, like everything here.
var managedBy = []byte(`{"kind":"service","name":"Example publisher","owner":{"id":"svc-example","name":"Example publisher"},"since":"2026-01-01T00:00:00Z","yours":false}`)

// URL is the fake drive's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Token is a valid mount token.
func (s *Server) Token() string { return testToken }

// RootID is the drive root's id.
func (s *Server) RootID() string { return s.rootID }

// Client is the underlying HTTP client to use with the fake.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Close stops the server and ends every stream.
func (s *Server) Close() {
	s.DropStreams()
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// Count returns how often a thing happened: "upload.start", "upload.complete",
// "part.put" (and "part.put.multipart" for large files' parts), "download", "children", "changes", "folder.create", "patch", "trash".
func (s *Server) Count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[name]
}

// LastHeartbeat returns the last heartbeat received.
func (s *Server) LastHeartbeat() *driveapi.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heartbeat
}

// ---- seeding and inspection ----

// AddFolder creates a folder and returns its id. It panics on a bad parent.
func (s *Server) AddFolder(parentID, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.addNodeLocked(parentID, driveapi.KindFolder, name)
	return n.id
}

// AddFile creates a file with data and returns its id.
func (s *Server) AddFile(parentID, name string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.addNodeLocked(parentID, driveapi.KindFile, name)
	s.addVersionLocked(n, data, "", nil)
	return n.id
}

// PutVersion stores a new version of a file, as if someone else saved it.
func (s *Server) PutVersion(fileID string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[fileID]
	if n == nil {
		panic("fakedrive: no file " + fileID)
	}
	return s.addVersionLocked(n, data, "", nil).id
}

// Rename renames a node as another client would.
func (s *Server) Rename(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	n.name = name
	n.updated = time.Now().UTC()
	s.emitLocked(id, "")
}

// Trash moves a node to the trash as another client would.
func (s *Server) Trash(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := time.Now().UTC()
	s.nodes[id].trashed = &t
	s.emitLocked(id, "")
}

// Purge deletes a node for good (a "removed" change).
func (s *Server) Purge(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	s.emitLocked(id, "")
}

// FileInfo is a stored file as tests see it.
type FileInfo struct {
	ID         string
	Data       []byte
	Versions   int
	VersionID  string
	ModifiedAt *time.Time
	Trashed    bool
	// SHA256 and ChecksumState of the current version.
	SHA256        string
	ChecksumState string
}

// Lookup finds a live node by path ("/a/b.txt", case-insensitive).
func (s *Server) Lookup(p string) (FileInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.resolveLocked(p)
	if n == nil {
		return FileInfo{}, false
	}
	fi := FileInfo{ID: n.id, Versions: len(n.versions), ModifiedAt: n.modifiedAt, Trashed: n.trashed != nil}
	if v := n.current(); v != nil {
		fi.Data = v.content.bytes()
		fi.VersionID = v.id
		fi.SHA256, fi.ChecksumState = v.sha256, v.checksumState
	}
	return fi, true
}

// Get returns a node by id, trashed or not.
func (s *Server) Get(id string) (FileInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil {
		return FileInfo{}, false
	}
	fi := FileInfo{ID: n.id, Versions: len(n.versions), ModifiedAt: n.modifiedAt, Trashed: n.trashed != nil}
	if v := n.current(); v != nil {
		fi.Data = v.content.bytes()
		fi.VersionID = v.id
		fi.SHA256, fi.ChecksumState = v.sha256, v.checksumState
	}
	return fi, true
}

// List returns the names of a folder's live children, sorted.
func (s *Server) List(folderID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, c := range s.childrenLocked(folderID) {
		names = append(names, c.name)
	}
	return names
}

// TreeEntry is one live node below a folder, as the fault controls' /tree
// lists it for scripts that compare the drive with a mounted copy.
type TreeEntry struct {
	Path     string `json:"path"` // below the folder, "/"-separated
	Kind     string `json:"kind"` // file or folder
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Versions int    `json:"versions"`
	SHA256   string `json:"sha256,omitempty"`
}

// Tree lists every live node below the folder at path p, folders before
// their contents. Hashes not yet known are worked out (synthetic data only).
func (s *Server) Tree(p string) ([]TreeEntry, bool) {
	s.mu.Lock()
	top := s.resolveLocked(p)
	if top == nil || top.kind != driveapi.KindFolder {
		s.mu.Unlock()
		return nil, false
	}
	type pending struct {
		e TreeEntry
		v *version
	}
	var out []pending
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		for _, c := range s.childrenLocked(id) {
			e := TreeEntry{Path: prefix + c.name, Kind: c.kind, ID: c.id, Versions: len(c.versions)}
			v := c.current()
			if v != nil {
				e.Size, e.SHA256 = v.content.size(), v.sha256
			}
			out = append(out, pending{e, v})
			if c.kind == driveapi.KindFolder {
				walk(c.id, e.Path+"/")
			}
		}
	}
	walk(top.id, "")
	s.mu.Unlock()
	entries := make([]TreeEntry, len(out))
	for i, p := range out {
		if p.e.Kind == driveapi.KindFile && p.e.SHA256 == "" && p.v != nil {
			p.e.SHA256 = p.v.content.sha256()
		}
		entries[i] = p.e
	}
	return entries, true
}

// PendingUploads is how many uploads are unfinished.
func (s *Server) PendingUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// AddVirtualFile creates a file of size bytes whose content is generated on
// read (see VirtualBytes): it takes no memory or disk. Its SHA-256 is worked
// out in the background and is empty until then.
func (s *Server) AddVirtualFile(parentID, name string, size int64) string {
	s.mu.Lock()
	n := s.addNodeLocked(parentID, driveapi.KindFile, name)
	v := s.addBlobLocked(n, blob{virtual: size}, "", "", nil)
	s.mu.Unlock()
	go func() {
		sum := v.content.sha256()
		s.mu.Lock()
		v.sha256 = sum
		s.mu.Unlock()
	}()
	return n.id
}

// Stats is a snapshot for the fault controls and tests.
type Stats struct {
	Folders          int            `json:"folders"`
	Files            int            `json:"files"`
	Bytes            int64          `json:"bytes"`
	PendingUploads   int            `json:"pending_uploads"`
	SignedInDevices  int            `json:"signed_in_devices"`
	OpenStreams      int            `json:"open_streams"`
	Offline          bool           `json:"offline"`
	UnavailableUntil *time.Time     `json:"unavailable_until,omitempty"`
	Counts           map[string]int `json:"counts"`
}

// Stats counts what the drive holds and what it has been asked.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{PendingUploads: len(s.uploads), SignedInDevices: len(s.devices), OpenStreams: len(s.streams),
		Offline: s.faults.offline, Counts: map[string]int{}}
	for k, v := range s.counts {
		st.Counts[k] = v
	}
	if u := s.faults.unavailableUntil; time.Now().Before(u) {
		st.UnavailableUntil = &u
	}
	for _, n := range s.nodes {
		if n.trashed != nil || n.id == s.rootID {
			continue
		}
		if n.kind == driveapi.KindFolder {
			st.Folders++
		} else {
			st.Files++
			st.Bytes += n.size()
		}
	}
	return st
}

// CorruptNextUploads makes the next n completed uploads behave as if
// storage held other bytes than were sent: an upload the drive checks at
// once is refused with checksum_mismatch; a larger one is published as
// claimed and the checksum pass then reports "mismatch".
func (s *Server) CorruptNextUploads(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults.corruptUploads = n
}

// RunChecksumPass checks every claimed version, as the real drive's worker
// does: "verified" with the SHA-256 set, or "mismatch". Each result shows
// up in the changes feed as an upsert. It returns how many it checked.
func (s *Server) RunChecksumPass() int {
	type job struct {
		n *node
		v *version
	}
	s.mu.Lock()
	var jobs []job
	for _, n := range s.nodes {
		for _, v := range n.versions {
			if v.checksumState == driveapi.ChecksumClaimed {
				jobs = append(jobs, job{n, v})
			}
		}
	}
	s.mu.Unlock()
	for _, j := range jobs {
		sum := j.v.content.sha256() // outside the lock: it reads all the bytes
		s.mu.Lock()
		if j.v.checksumState == driveapi.ChecksumClaimed {
			if sum == j.v.claimed && !j.v.corrupt {
				j.v.checksumState, j.v.sha256 = driveapi.ChecksumVerified, sum
				s.counts["checksum.verified"]++
			} else {
				j.v.checksumState = driveapi.ChecksumMismatch
				s.counts["checksum.mismatch"]++
			}
			s.emitLocked(j.n.id, "")
		}
		s.mu.Unlock()
	}
	return len(jobs)
}

// RevokeAll revokes every token, linked devices and the built-in test token.
func (s *Server) RevokeAll() {
	s.mu.Lock()
	clear(s.tokens)
	clear(s.devices)
	s.mu.Unlock()
	s.DropStreams()
}

// ---- faults ----

// SetLatency adds d before every answer.
func (s *Server) SetLatency(d time.Duration) {
	s.mu.Lock()
	s.faults.latency = d
	s.mu.Unlock()
}

// Unavailable answers every API route (not storage) with 503 and
// Retry-After: 1 for d, as during a deploy, and ends open streams.
func (s *Server) Unavailable(d time.Duration) {
	s.mu.Lock()
	s.faults.unavailableUntil = time.Now().Add(d)
	s.mu.Unlock()
	s.DropStreams()
}

// SetOffline makes every request (API and storage) fail with a dropped
// connection, as when the network is gone.
func (s *Server) SetOffline(on bool) {
	s.mu.Lock()
	s.faults.offline = on
	s.mu.Unlock()
	if on {
		s.DropStreams()
		s.srv.CloseClientConnections()
	}
}

// DropNextDownloads cuts the next n content downloads after "after" bytes.
func (s *Server) DropNextDownloads(n int, after int64) {
	s.mu.Lock()
	s.faults.dropDownloads, s.faults.dropDownloadAfter = n, after
	s.mu.Unlock()
}

// DropNextPartPuts cuts the next n part uploads after reading "after" bytes.
func (s *Server) DropNextPartPuts(n int, after int64) {
	s.mu.Lock()
	s.faults.dropPuts, s.faults.dropPutAfter = n, after
	s.mu.Unlock()
}

// ExpirePartURLs makes every presigned URL handed out so far answer 403.
func (s *Server) ExpirePartURLs() {
	s.mu.Lock()
	s.sigs = map[string]time.Time{}
	s.mu.Unlock()
}

// RevokeToken unlinks the device: its token answers 401 from now on.
func (s *Server) RevokeToken(tok string) {
	s.mu.Lock()
	delete(s.tokens, tok)
	s.mu.Unlock()
	s.DropStreams()
}

// RefuseWrites makes every write inside folderID fail with code: "managed"
// (423), "sealed" (403), "forbidden" (403), "legal_hold" (423),
// "writes_paused" (423). "" lifts it.
func (s *Server) RefuseWrites(folderID, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		delete(s.refuse, folderID)
		return
	}
	s.refuse[folderID] = code
}

// SetQuota limits the bytes stored under folderID (0 lifts it).
func (s *Server) SetQuota(folderID string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes <= 0 {
		delete(s.quota, folderID)
		return
	}
	s.quota[folderID] = bytes
}

// ForceReset makes the next changes call answer a reset with reason.
func (s *Server) ForceReset(reason string) {
	s.mu.Lock()
	s.faults.reset = reason
	s.mu.Unlock()
}

// EmitResync records a "resync" change for folderID.
func (s *Server) EmitResync(folderID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitLocked(folderID, driveapi.OpResync)
}

// SetIntercept installs a hook that sees every request first; returning
// true means it answered.
func (s *Server) SetIntercept(fn func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	s.intercept = fn
	s.mu.Unlock()
}

// SetRelease serves a signed manifest on channel.
func (s *Server) SetRelease(channel string, body []byte) {
	s.mu.Lock()
	s.release[channel] = body
	s.mu.Unlock()
}

// DropStreams ends every open SSE stream.
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		close(ch)
		delete(s.streams, ch)
	}
}

// ---- internals ----

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func randToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (n *node) current() *version {
	if len(n.versions) == 0 {
		return nil
	}
	return n.versions[len(n.versions)-1]
}

func (n *node) size() int64 {
	if v := n.current(); v != nil {
		return v.content.size()
	}
	return 0
}

func (s *Server) addNodeLocked(parentID, kind, name string) *node {
	p := s.nodes[parentID]
	if p == nil || p.kind != driveapi.KindFolder {
		panic("fakedrive: no folder " + parentID)
	}
	now := time.Now().UTC()
	n := &node{id: newID(), parentID: parentID, kind: kind, name: name, created: now, updated: now}
	s.nodes[n.id] = n
	s.emitLocked(n.id, "")
	return n
}

func (s *Server) addVersionLocked(n *node, data []byte, fingerprint string, mtime *time.Time) *version {
	sum := sha256.Sum256(data)
	return s.addBlobLocked(n, memBlob(data), hex.EncodeToString(sum[:]), fingerprint, mtime)
}

func (s *Server) addBlobLocked(n *node, content blob, sha, fingerprint string, mtime *time.Time) *version {
	v := &version{id: newID(), number: len(n.versions) + 1, content: content, sha256: sha,
		fingerprint: fingerprint, created: time.Now().UTC(), modifiedAt: mtime}
	n.versions = append(n.versions, v)
	n.updated = v.created
	if mtime != nil {
		n.modifiedAt = mtime
	}
	s.emitLocked(n.id, "")
	return v
}

func (s *Server) emitLocked(nodeID, op string) {
	s.seq++
	s.events = append(s.events, event{seq: s.seq, nodeID: nodeID, op: op})
	parent := ""
	if n := s.nodes[nodeID]; n != nil {
		parent = n.parentID
	}
	msg := fmt.Sprintf(`{"type":"file.updated","node_id":%q,"parent_id":%q,"seq":%d}`, nodeID, parent, s.seq)
	for ch := range s.streams {
		select {
		case ch <- msg:
		default: // slow client misses events, as the real stream allows
		}
	}
}

// live reports a node that exists, is not trashed and has no trashed ancestor.
func (s *Server) liveLocked(n *node) bool {
	for n != nil {
		if n.trashed != nil {
			return false
		}
		if n.parentID == "" {
			return n.id == s.rootID
		}
		n = s.nodes[n.parentID]
	}
	return false
}

func (s *Server) childrenLocked(folderID string) []*node {
	var out []*node
	for _, n := range s.nodes {
		if n.parentID == folderID && n.trashed == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].kind == driveapi.KindFolder) != (out[j].kind == driveapi.KindFolder) {
			return out[i].kind == driveapi.KindFolder
		}
		return strings.ToLower(out[i].name) < strings.ToLower(out[j].name)
	})
	return out
}

func (s *Server) childByNameLocked(folderID, name string) *node {
	for _, n := range s.nodes {
		if n.parentID == folderID && n.trashed == nil && strings.EqualFold(n.name, name) {
			return n
		}
	}
	return nil
}

func (s *Server) resolveLocked(p string) *node {
	n := s.nodes[s.rootID]
	for _, part := range strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if part == "" {
			continue
		}
		n = s.childByNameLocked(n.id, part)
		if n == nil {
			return nil
		}
	}
	return n
}

// refusalLocked returns the refusal code for a write inside folderID.
func (s *Server) refusalLocked(id string) string {
	for n := s.nodes[id]; n != nil; n = s.nodes[n.parentID] {
		if code, ok := s.refuse[n.id]; ok {
			return code
		}
		if n.parentID == "" {
			break
		}
	}
	return ""
}

func (s *Server) usedBytesLocked(folderID string) int64 {
	var total int64
	for _, n := range s.nodes {
		if n.kind != driveapi.KindFile || n.trashed != nil {
			continue
		}
		for p := n; p != nil; p = s.nodes[p.parentID] {
			if p.id == folderID {
				total += n.size()
				break
			}
			if p.parentID == "" {
				break
			}
		}
	}
	return total
}

// quotaLocked reports whether adding bytes under parentID breaks a quota.
func (s *Server) quotaLocked(parentID string, add int64) bool {
	for n := s.nodes[parentID]; n != nil; n = s.nodes[n.parentID] {
		if q, ok := s.quota[n.id]; ok && s.usedBytesLocked(n.id)+add > q {
			return true
		}
		if n.parentID == "" {
			break
		}
	}
	return false
}

func (s *Server) toAPILocked(n *node) driveapi.Node {
	out := driveapi.Node{ID: n.id, ParentID: n.parentID, Kind: n.kind, Name: n.name, Size: n.size(),
		CreatedAt: n.created, UpdatedAt: n.updated, Role: "manager",
		Can: driveapi.Can{Read: true, Upload: true, Write: true, Delete: true, Share: true, Manage: true},
		URI: s.srv.URL + "/n/" + n.id, TrashedAt: n.trashed}
	if n.modifiedAt != nil {
		out.ModifiedAt = driveapi.NewTime(*n.modifiedAt)
	}
	switch s.refusalLocked(n.id) {
	case driveapi.CodeSealed:
		out.Sealed = true
		out.Can.Upload, out.Can.Delete = false, false
	case driveapi.CodeManaged:
		if n.kind == driveapi.KindFile {
			out.Managed = managedBy
			out.Can.Write, out.Can.Delete = false, false
		}
	}
	if v := n.current(); v != nil {
		out.CurrentVersion = &driveapi.Version{ID: v.id, Number: v.number, Size: v.content.size(), SHA256: v.sha256,
			ChecksumState: v.checksumState, CreatedAt: v.created}
		if v.modifiedAt != nil {
			out.CurrentVersion.ModifiedAt = driveapi.NewTime(*v.modifiedAt)
		}
	}
	return out
}

func validName(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func freeName(s *Server, parentID, name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if s.childByNameLocked(parentID, cand) == nil {
			return cand
		}
	}
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
