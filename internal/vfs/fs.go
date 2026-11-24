package vfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// FS is the drive's file system.
type FS struct {
	cfg   Config
	api   *driveapi.Client
	eng   transfer.Engine
	idx   *index.DB
	cache *cache.Cache
	jr    *journal.Journal
	log   *slog.Logger

	// treeMu serialises changes to the index (local operations, listings,
	// the changes feed) so read-modify-write sequences do not interleave.
	treeMu sync.Mutex

	// finished counts changes that reached the drive; touched holds, by
	// drive id, the count when each node was last changed from here (see
	// changedSince). touched is guarded by mu.
	finished atomic.Uint64
	touched  map[string]touchedAt

	mu      sync.Mutex
	open    map[string]*openNode
	pending map[string]int // local id -> queued journal ops
	// deleting: queued deletes, by drive id, or by folder and name (see
	// deleteKeys); what the drive still shows of them is not brought back
	deleting map[string]int
	inflight map[uint64]*running
	listing  map[string]*listCall
	state    State
	subs     map[int]chan Event
	nextSub  int

	// displaced: drive files just renamed to a temp name, by folder and old
	// name (see takeDisplaced); guarded by mu.
	displaced map[string]displacedFile

	// upMu guards uploading; it is a leaf lock (nothing is locked under it).
	upMu      sync.Mutex
	uploading map[string]string // local id -> content key being uploaded

	// release: the timings of the release rules; guarded by mu.
	release cache.ReleasePolicy

	kick chan struct{} // wake the dispatcher
	wake chan struct{} // wake the changes loop

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type running struct {
	op        journal.Op
	cancel    context.CancelFunc
	cancelled bool
}

type touchedAt struct {
	gen uint64
	at  time.Time
}

type listCall struct {
	done chan struct{}
	err  error
	// again: the folder changed while this listing was in flight (a resync
	// or a reset), so its answer may be stale: fetch once more before done.
	// Guarded by FS.mu.
	again bool
}

// New opens the file system over the given parts. It replays the journal's
// bookkeeping (nothing is sent until Start).
func New(cfg Config) (*FS, error) {
	if cfg.API == nil || cfg.Engine == nil || cfg.Index == nil || cfg.Cache == nil || cfg.Journal == nil || cfg.RootID == "" {
		return nil, errors.New("vfs: config needs API, Engine, Index, Cache, Journal and RootID")
	}
	if cfg.ListTimeout <= 0 {
		cfg.ListTimeout = 2 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.ReadAhead <= 0 {
		cfg.ReadAhead = 4 << 20
	}
	if cfg.PCName == "" {
		cfg.PCName = "this PC"
	}
	if cfg.VolumeLabel == "" {
		cfg.VolumeLabel = "Olsyn Drive"
	}
	if cfg.VolumeTotal <= 0 {
		cfg.VolumeTotal = 2 << 40
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ReleaseInterval <= 0 {
		cfg.ReleaseInterval = 30 * time.Second
	}
	fs := &FS{release: cfg.Release.WithDefaults(), cfg: cfg, api: cfg.API, eng: cfg.Engine, idx: cfg.Index, cache: cfg.Cache, jr: cfg.Journal, log: cfg.Logger,
		open: map[string]*openNode{}, pending: map[string]int{}, deleting: map[string]int{}, touched: map[string]touchedAt{}, inflight: map[uint64]*running{}, displaced: map[string]displacedFile{},
		uploading: map[string]string{}, listing: map[string]*listCall{}, state: StateOnline,
		subs: map[int]chan Event{}, kick: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	if err := fs.recover(); err != nil {
		return nil, err
	}
	return fs, nil
}

// recover makes sure the root exists and the index agrees with the journal
// after a crash between writing one and the other.
func (fs *FS) recover() error {
	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()
	if root, err := fs.idx.Get(fs.cfg.RootID); err != nil || root.ParentID != "" {
		if err := fs.idx.Put(&index.Entry{ID: fs.cfg.RootID, ServerID: fs.cfg.RootID, Kind: index.KindFolder,
			CanUpload: true, CanWrite: true}); err != nil {
			return fmt.Errorf("vfs: create root: %w", err)
		}
	}
	ops, err := fs.jr.List()
	if err != nil {
		return fmt.Errorf("vfs: read journal: %w", err)
	}
	deleted := map[string]bool{}
	for _, op := range ops {
		if op.Kind == journal.OpDelete {
			deleted[op.NodeID] = true
		}
	}
	for _, op := range ops {
		fs.pending[op.NodeID]++
		for _, k := range deleteKeys(op) {
			fs.deleting[k]++
		}
		if op.Kind != journal.OpUpload && op.Kind != journal.OpMkdir {
			continue
		}
		e, err := fs.idx.Get(op.NodeID)
		if errors.Is(err, index.ErrNotFound) {
			if deleted[op.NodeID] {
				continue // deleted here: the op only cleans up on the drive
			}
			// queued but the index write was lost: rebuild the local entry
			if _, perr := fs.idx.Get(op.ParentID); perr != nil {
				continue
			}
			e = &index.Entry{ID: op.NodeID, ParentID: op.ParentID, Name: op.Name, Kind: index.KindFile,
				MTime: op.MTime, Created: op.Queued, CanWrite: true, CanUpload: true, CanDelete: true}
			if op.IsDir {
				e.Kind = index.KindFolder
			}
		} else if err != nil {
			return err
		}
		damaged := false
		if op.Kind == journal.OpUpload && op.ContentKey != "" && !fs.cache.Has(op.ContentKey) {
			// data survived, metadata did not; a small file's data may not
			// have reached the disk before a power loss: check it
			if fs.cache.Adopt(op.ContentKey, op.Size) == nil && op.CRC != 0 {
				damaged = !fs.contentMatches(op.ContentKey, op.CRC)
			}
		}
		if op.Kind == journal.OpUpload && e.ContentKey == "" && op.ContentKey != "" && fs.cache.Has(op.ContentKey) {
			e.ContentKey, e.Size = op.ContentKey, op.Size
		}
		if perr := fs.idx.Put(e); perr != nil {
			fs.log.Warn("vfs: recover entry", "err", perr)
		}
		if op.Kind == journal.OpUpload && op.State == journal.StatePending && !fs.cache.Has(op.ContentKey) && e.ContentKey == op.ContentKey {
			op.State, op.HeldCode, op.Reason = journal.StateHeld, "local_copy_lost", "The copy on this PC was lost before it was sent."
			_ = fs.jr.Update(op)
		}
		if damaged && op.State == journal.StatePending {
			fs.log.Warn("vfs: saved file damaged by a power loss before it was sent", "seq", op.Seq)
			op.State, op.HeldCode, op.Reason = journal.StateHeld, "local_copy_lost",
				"The copy on this PC was damaged (the PC lost power before it was fully saved), so it was not sent."
			_ = fs.jr.Update(op)
		}
	}
	fs.cache.DropOrphans()
	return nil
}

// contentMatches reports whether a cache entry's content has the CRC it
// was queued with.
func (fs *FS) contentMatches(key string, crc uint32) bool {
	h, err := fs.cache.Open(key)
	if err != nil {
		return false
	}
	defer h.Close()
	sum, err := contentCRC(h)
	return err == nil && sum == crc
}

// Start runs the background work: the changes feed and its wake-ups, the
// upload queue and cache flushing. Stop ends it.
func (fs *FS) Start(ctx context.Context) {
	fs.ctx, fs.cancel = context.WithCancel(ctx)
	loops := []func(){fs.dispatchLoop, fs.changesLoop, fs.flushLoop, fs.releaseLoop}
	if !fs.cfg.DisableStream {
		loops = append(loops, fs.streamLoop)
	}
	for _, l := range loops {
		fs.wg.Add(1)
		go func() {
			defer fs.wg.Done()
			l()
		}()
	}
	fs.kickNow()
	fs.wakeNow()
}

// Stop ends the background work at once (in-flight transfers are cut, the
// journal keeps them) and waits for it. It does not flush: Close does.
func (fs *FS) Stop() {
	if fs.cancel != nil {
		fs.cancel()
	}
	fs.wg.Wait()
	fs.mu.Lock()
	for id, ch := range fs.subs {
		close(ch)
		delete(fs.subs, id)
	}
	fs.mu.Unlock()
}

// Close stops the background work and flushes the cache. The caller closes
// the index, cache and journal.
func (fs *FS) Close() error {
	fs.Stop()
	return fs.cache.Flush()
}

func (fs *FS) kickNow() {
	select {
	case fs.kick <- struct{}{}:
	default:
	}
}

func (fs *FS) wakeNow() {
	select {
	case fs.wake <- struct{}{}:
	default:
	}
}

func newLocalID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// ---- state and events ----

// State is the current connection state.
func (fs *FS) State() State {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.state
}

func (fs *FS) setState(s State) {
	fs.mu.Lock()
	if fs.state == s {
		fs.mu.Unlock()
		return
	}
	fs.state = s
	fs.mu.Unlock()
	fs.log.Info("vfs: state", "state", string(s))
	fs.emit(Event{Kind: EventState, State: s})
}

// noteErr records what a failed drive call says about the connection.
func (fs *FS) noteErr(err error) {
	switch {
	case err == nil:
	case driveapi.IsAuthLost(err):
		fs.setState(StateSignedOut)
	case driveapi.IsNetwork(err), driveapi.IsRetryable(err) && !driveapi.IsWait(err):
		if !errors.Is(err, context.Canceled) {
			fs.setState(StateOffline)
		}
	case driveapi.IsRetryable(err):
		// the drive answered and asked us to wait: it is reachable
	}
}

func (fs *FS) noteOK() {
	if fs.State() == StateOffline {
		fs.setState(StateOnline)
	}
}

func (fs *FS) offline() bool {
	s := fs.State()
	return s == StateOffline || s == StateSignedOut
}

// Subscribe returns a channel of events. Slow readers miss events (State,
// Held and the transfer list can always be read directly). Call cancel to
// stop.
func (fs *FS) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	ch := make(chan Event, buffer)
	fs.mu.Lock()
	id := fs.nextSub
	fs.nextSub++
	fs.subs[id] = ch
	fs.mu.Unlock()
	return ch, func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if c, ok := fs.subs[id]; ok {
			close(c)
			delete(fs.subs, id)
		}
	}
}

func (fs *FS) emit(e Event) {
	e.At = time.Now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, ch := range fs.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// TransferEvent forwards an engine event to subscribers; wire it as the
// engine's OnEvent.
func (fs *FS) TransferEvent(e transfer.Event) {
	var p string
	if seq, err := strconv.ParseUint(e.TransferID, 10, 64); err == nil {
		fs.mu.Lock()
		r := fs.inflight[seq]
		fs.mu.Unlock()
		if r != nil {
			p = fs.pathOf(r.op.NodeID)
		}
	}
	fs.emit(Event{Kind: EventTransfer, Transfer: &e, Path: p, Seq: 0})
}

// ---- tree access ----

func (fs *FS) isPending(id string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.pendingLocked(id)
}

func (fs *FS) pendingLocked(id string) bool {
	if fs.pending[id] > 0 {
		return true
	}
	if on := fs.open[id]; on != nil && on.hasLocalChanges() {
		return true
	}
	return false
}

// isPendingTree reports a node that has, or is an ancestor of, unsent changes.
func (fs *FS) isPendingTree(tx *index.Tx, id string) bool {
	fs.mu.Lock()
	var ids []string
	for n, c := range fs.pending {
		if c > 0 {
			ids = append(ids, n)
		}
	}
	for n, on := range fs.open {
		if on.hasLocalChanges() {
			ids = append(ids, n)
		}
	}
	fs.mu.Unlock()
	for _, n := range ids {
		for cur, depth := n, 0; cur != "" && depth < 4096; depth++ {
			if cur == id {
				return true
			}
			e, err := tx.Get(cur)
			if err != nil {
				break
			}
			cur = e.ParentID
		}
	}
	return false
}

// resolve walks an OS path to its entry, listing folders on the way.
func (fs *FS) resolve(ctx context.Context, path string) (*index.Entry, error) {
	return fs.walk(ctx, splitPath(path), path)
}

func (fs *FS) walk(ctx context.Context, parts []string, path string) (*index.Entry, error) {
	e, err := fs.idx.Get(fs.cfg.RootID)
	if err != nil {
		return nil, newErr(KindOther, "lookup", path, err)
	}
	for _, p := range parts {
		if !e.IsDir() {
			return nil, newErr(KindNotDir, "lookup", path, nil)
		}
		if err := fs.ensureListed(ctx, e); err != nil {
			return nil, err
		}
		child, err := fs.idx.Lookup(e.ID, DecodeName(p))
		if errors.Is(err, index.ErrNotFound) {
			return nil, newErr(KindNotFound, "lookup", path, nil)
		}
		if err != nil {
			return nil, newErr(KindOther, "lookup", path, err)
		}
		e = child
	}
	return e, nil
}

// ensureListed makes sure a folder's children are in the index, fetching
// them once with a bounded wait. The fetch carries on in the background
// when the wait runs out (KindTimeout: try again shortly).
func (fs *FS) ensureListed(ctx context.Context, dir *index.Entry) error {
	if dir.ServerID == "" || dir.LocalOnly || fs.idx.Listed(dir.ID) {
		return nil
	}
	if fs.offline() {
		return newErr(KindOffline, "list", dir.Name, nil)
	}
	call := fs.startListing(dir)
	t := time.NewTimer(fs.cfg.ListTimeout)
	defer t.Stop()
	select {
	case <-call.done:
		return classify("list", dir.Name, call.err)
	case <-t.C:
		return newErr(KindTimeout, "list", dir.Name, errors.New("folder still loading"))
	case <-ctx.Done():
		return classify("list", dir.Name, ctx.Err())
	}
}

func (fs *FS) startListing(dir *index.Entry) *listCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if c, ok := fs.listing[dir.ID]; ok {
		return c
	}
	c := &listCall{done: make(chan struct{})}
	fs.listing[dir.ID] = c
	ctx := fs.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		for {
			c.err = fs.fetchListing(ctx, dir.ID, dir.ServerID)
			fs.mu.Lock()
			if c.again && c.err == nil && ctx.Err() == nil {
				c.again = false
				fs.mu.Unlock()
				continue
			}
			delete(fs.listing, dir.ID)
			fs.mu.Unlock()
			close(c.done)
			return
		}
	}()
	return c
}

// fetchListing reads a folder's children from the drive into the index.
// Children the drive did not list are dropped. What the answer says about
// a node one of this PC's changes reached the drive for while the answer
// was on its way (a folder made, a file sent, moved in or out, renamed or
// trashed just after the drive answered) is older than what the index
// holds: it is skipped (see changedSince).
func (fs *FS) fetchListing(ctx context.Context, id, sid string) error {
	before := fs.finished.Load()
	kids, err := fs.api.AllChildren(ctx, sid)
	if err != nil {
		fs.noteErr(err)
		if driveapi.IsNotFound(err) {
			return newErr(KindNotFound, "list", "", err)
		}
		return err
	}
	fs.noteOK()
	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()
	return fs.idx.Update(func(tx *index.Tx) error {
		if _, err := tx.Get(id); err != nil {
			return nil // removed meanwhile
		}
		existing, err := tx.Children(id)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for i := range kids {
			seen[kids[i].ID] = true
			if fs.changedSince(kids[i].ID, before) {
				continue
			}
			if err := fs.mergeNode(tx, &kids[i], id); err != nil {
				return err
			}
		}
		for _, c := range existing {
			if c.ServerID == "" || seen[c.ServerID] || c.LocalOnly || fs.isPendingTree(tx, c.ID) || fs.changedSince(c.ServerID, before) {
				continue
			}
			if _, err := tx.Delete(c.ID); err != nil && !errors.Is(err, index.ErrNotFound) {
				return err
			}
		}
		return tx.SetListed(id, true)
	})
}

// touchedKeep is how long a node's last change is remembered for
// changedSince: far longer than any answer takes to arrive.
const touchedKeep = 10 * time.Minute

// touch records that a change of this PC reached the drive for node sid.
func (fs *FS) touch(sids ...string) {
	gen := fs.finished.Add(1)
	now := time.Now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, sid := range sids {
		if sid != "" {
			fs.touched[sid] = touchedAt{gen: gen, at: now}
		}
	}
	if gen%1024 == 0 {
		for sid, t := range fs.touched {
			if now.Sub(t.at) > touchedKeep {
				delete(fs.touched, sid)
			}
		}
	}
}

// changedSince reports a node one of this PC's changes reached the drive
// for after the finished count was before: an answer asked for before
// then may describe it as it was.
func (fs *FS) changedSince(sid string, before uint64) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t, ok := fs.touched[sid]
	return ok && t.gen > before
}

// relist refreshes a folder in the background (after a resync). A listing
// already in flight may have been answered before the change, so it runs
// again rather than being reused.
func (fs *FS) relist(id string) *listCall {
	e, err := fs.idx.Get(id)
	if err != nil || e.ServerID == "" || e.LocalOnly {
		return nil
	}
	fs.mu.Lock()
	if c, ok := fs.listing[id]; ok {
		c.again = true
		fs.mu.Unlock()
		return c
	}
	fs.mu.Unlock()
	return fs.startListing(e)
}

// mergeNode writes what the drive says about a node into the index, under
// parent (a local id). Local changes not yet sent win over the drive's
// name, size and content until they are sent.
func (fs *FS) mergeNode(tx *index.Tx, n *driveapi.Node, parent string) error {
	if n.Kind != driveapi.KindFile && n.Kind != driveapi.KindFolder {
		return nil // shortcuts are not shown on the mount yet
	}
	if n.ID == fs.cfg.RootID {
		return nil
	}
	if fs.beingDeleted(n.ID, parent, n.Name) {
		// deleted here, not yet in the drive's trash: an answer or feed
		// entry from before must not bring it back
		return nil
	}
	ex, err := tx.BySID(n.ID)
	if err != nil && !errors.Is(err, index.ErrNotFound) {
		return err
	}
	if ex != nil && fs.isPending(ex.ID) {
		ex.CanWrite, ex.CanUpload, ex.CanDelete = n.Can.Write, n.Can.Upload, n.Can.Delete
		ex.Sealed, ex.Managed = n.Sealed, n.IsManaged()
		return tx.Put(ex)
	}
	e := ex
	if e == nil {
		e = &index.Entry{ID: n.ID}
	}
	e.ServerID, e.ParentID, e.Name, e.Kind = n.ID, parent, n.Name, n.Kind
	e.Size, e.MTime, e.Created = n.Size, n.MTime(), n.CreatedAt
	e.VersionID = n.VersionID()
	e.SHA256 = ""
	if n.CurrentVersion != nil {
		e.SHA256 = n.CurrentVersion.SHA256
	}
	e.CanWrite, e.CanUpload, e.CanDelete = n.Can.Write, n.Can.Upload, n.Can.Delete
	e.Sealed, e.Managed = n.Sealed, n.IsManaged()
	if e.IsDir() {
		e.Size = 0
	}
	if other, err := tx.Lookup(parent, n.Name); err == nil && other.ID != e.ID {
		if other.LocalOnly || other.ServerID == "" || fs.isPending(other.ID) {
			// our unsent file holds the name; the drive's shows up once ours
			// is resolved (sent, or saved as a conflict copy)
			if ex != nil {
				return nil
			}
			return nil
		}
		// a stale entry holds the name (renamed on the drive, change not
		// applied yet): park it until its own change arrives
		other.Name = "\x00" + other.ID
		if err := tx.Put(other); err != nil {
			return err
		}
	}
	return tx.Put(e)
}

// writable rules, checked locally so refusals are immediate.
func canCreateIn(dir *index.Entry) bool {
	return dir.ServerID == "" || dir.LocalOnly || (dir.CanUpload && !dir.Sealed)
}

func canModify(e *index.Entry) bool {
	return e.ServerID == "" || e.LocalOnly || (!e.Sealed && !e.Managed && e.CanWrite)
}

func canRemove(e *index.Entry) bool {
	return e.ServerID == "" || e.LocalOnly || (!e.Sealed && !e.Managed && e.CanDelete)
}

// info builds the OS view of an entry.
func (fs *FS) info(e *index.Entry) Info {
	fs.mu.Lock()
	on := fs.open[e.ID]
	pending := fs.pending[e.ID] > 0
	fs.mu.Unlock()
	if on != nil {
		cur := on.snapshot()
		cur.Attrs = e.Attrs | cur.Attrs
		e = &cur
	}
	in := Info{ID: e.ID, Name: EncodeName(e.Name), IsDir: e.IsDir(), Size: e.Size, MTime: e.MTime, CTime: e.Created,
		Attrs: e.Attrs, Pending: pending || e.ContentKey != ""}
	if in.CTime.IsZero() {
		in.CTime = in.MTime
	}
	if e.IsDir() {
		in.Size = 0
		in.ReadOnly = !canCreateIn(e)
	} else {
		in.ReadOnly = !canModify(e)
		in.Offline = e.ContentKey == "" && e.Size > 0 && !fs.cache.IsComplete(e.VersionID)
	}
	return in
}

// Stat implements FileSystem.
func (fs *FS) Stat(ctx context.Context, path string) (Info, error) {
	e, err := fs.resolve(ctx, path)
	if err != nil {
		return Info{}, err
	}
	return fs.info(e), nil
}

// ReadDir implements FileSystem: up to limit entries (0 = all) after the OS
// name marker, from the index only (a folder never listed is fetched once).
func (fs *FS) ReadDir(ctx context.Context, path, marker string, limit int) ([]Info, string, error) {
	e, err := fs.resolve(ctx, path)
	if err != nil {
		return nil, "", err
	}
	return fs.readDir(ctx, e, marker, limit)
}

func (fs *FS) readDir(ctx context.Context, e *index.Entry, marker string, limit int) ([]Info, string, error) {
	if !e.IsDir() {
		return nil, "", newErr(KindNotDir, "list", e.Name, nil)
	}
	if err := fs.ensureListed(ctx, e); err != nil {
		return nil, "", err
	}
	if marker != "" {
		marker = DecodeName(marker)
	}
	var out []Info
	for {
		page, next, err := fs.idx.List(e.ID, marker, limit)
		if err != nil {
			return nil, "", newErr(KindOther, "list", e.Name, err)
		}
		for _, c := range page {
			if strings.HasPrefix(c.Name, "\x00") {
				continue // parked
			}
			out = append(out, fs.info(c))
		}
		if next == "" {
			return out, "", nil
		}
		if limit > 0 && len(out) >= limit {
			return out, EncodeName(next), nil
		}
		marker = next
	}
}

// VolumeInfo implements FileSystem.
func (fs *FS) VolumeInfo(context.Context) Volume {
	st := fs.cache.Stats()
	free := fs.cfg.VolumeTotal - st.Unsent
	if free < 0 {
		free = 0
	}
	return Volume{Label: fs.cfg.VolumeLabel, Total: fs.cfg.VolumeTotal, Free: free}
}

// pathOf builds the OS path of an entry for events ("" when unknown).
func (fs *FS) pathOf(id string) string {
	var parts []string
	for cur, depth := id, 0; cur != "" && cur != fs.cfg.RootID && depth < 4096; depth++ {
		e, err := fs.idx.Get(cur)
		if err != nil {
			return ""
		}
		parts = append([]string{EncodeName(e.Name)}, parts...)
		cur = e.ParentID
	}
	return "/" + strings.Join(parts, "/")
}

// flushLoop makes cache bitmaps durable every few seconds.
func (fs *FS) flushLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-fs.ctx.Done():
			return
		case <-t.C:
			if err := fs.cache.FlushOlder(30 * time.Second); err != nil {
				fs.log.Warn("vfs: cache flush", "err", err)
			}
		}
	}
}
