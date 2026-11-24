package vfs

import (
	"context"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// openNode is the shared state of a file or folder while it is open.
type openNode struct {
	id string

	mu       sync.Mutex
	e        index.Entry
	handles  map[*Handle]struct{}
	content  *cache.Handle // local content being written (dirty)
	clean    *cache.Handle // drive content for reads
	written  bool          // content changed since last queued
	timesSet bool          // modified time set by the app
	attrsSet bool
	doomed   bool // delete when the last handle cleans up

	fetchMu sync.Mutex // one fetch at a time per file
}

func (on *openNode) snapshot() index.Entry {
	on.mu.Lock()
	defer on.mu.Unlock()
	e := on.e
	if on.content != nil {
		e.Size = on.content.Size()
	}
	return e
}

// smallFileMax: new files up to this size get a CRC in the journal (reading
// them back is cheap: they are in the OS cache); larger ones are synced.
const smallFileMax = 64 << 20

// contentCRC is the CRC-32C of a cache entry's whole content.
func contentCRC(h *cache.Handle) (uint32, error) {
	buf := make([]byte, min(h.Size(), 256<<10))
	var sum uint32
	for off := int64(0); off < h.Size(); {
		n, err := h.ReadAt(buf, off)
		sum = crc32.Update(sum, castagnoli, buf[:n])
		off += int64(n)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n == 0 {
			break
		}
	}
	return sum, nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func (on *openNode) hasLocalChanges() bool {
	on.mu.Lock()
	defer on.mu.Unlock()
	return on.written || on.timesSet || on.content != nil
}

// Handle is one open of a file or folder.
type Handle struct {
	fs      *FS
	node    *openNode
	path    string
	access  Access
	share   Share
	isDir   bool
	lastEnd int64

	mu            sync.Mutex
	deleteOnClose bool
	cleaned       bool
	closed        bool
	// unsynced: this handle queued changes that Close must make durable
	// (Cleanup writes the journal; Close waits for its fsync).
	unsynced bool
}

// shareConflict applies Windows share-mode rules between two opens.
func shareConflict(a Access, as Share, b Access, bs Share) bool {
	return (a&AccessRead != 0 && bs&ShareRead == 0) ||
		(a&AccessWrite != 0 && bs&ShareWrite == 0) ||
		(a&AccessDelete != 0 && bs&ShareDelete == 0) ||
		(b&AccessRead != 0 && as&ShareRead == 0) ||
		(b&AccessWrite != 0 && as&ShareWrite == 0) ||
		(b&AccessDelete != 0 && as&ShareDelete == 0)
}

func (fs *FS) attach(e *index.Entry, path string, access Access, share Share) (*Handle, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	on := fs.open[e.ID]
	if on == nil {
		on = &openNode{id: e.ID, e: *e, handles: map[*Handle]struct{}{}}
		fs.open[e.ID] = on
	}
	on.mu.Lock()
	defer on.mu.Unlock()
	for o := range on.handles {
		if o.isCleaned() {
			continue
		}
		if shareConflict(access, share, o.access, o.share) {
			if len(on.handles) == 0 {
				delete(fs.open, e.ID)
			}
			return nil, newErr(KindSharingViolation, "open", path, nil)
		}
	}
	h := &Handle{fs: fs, node: on, path: path, access: access, share: share, isDir: e.IsDir()}
	on.handles[h] = struct{}{}
	return h, nil
}

func (h *Handle) isCleaned() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleaned
}

// Open implements FileSystem: opens or creates a file or folder.
func (fs *FS) Open(ctx context.Context, path string, fl OpenFlags) (*Handle, error) {
	parts := splitPath(path)
	if len(parts) == 0 {
		root, err := fs.walk(ctx, nil, path)
		if err != nil {
			return nil, err
		}
		if fl.NonDirectory {
			return nil, newErr(KindIsDir, "open", path, nil)
		}
		if fl.Create && fl.Exclusive {
			return nil, newErr(KindExists, "open", path, nil)
		}
		return fs.attach(root, path, fl.Access, ShareAll)
	}
	parent, err := fs.walk(ctx, parts[:len(parts)-1], path)
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() {
		return nil, newErr(KindNotDir, "open", path, nil)
	}
	name := DecodeName(parts[len(parts)-1])
	if !validName(name) {
		return nil, newErr(KindInvalidName, "open", path, nil)
	}
	if err := fs.ensureListed(ctx, parent); err != nil {
		return nil, err
	}
	e, err := fs.idx.Lookup(parent.ID, name)
	switch {
	case err == nil:
		return fs.openExisting(ctx, e, path, fl)
	case !errors.Is(err, index.ErrNotFound):
		return nil, newErr(KindOther, "open", path, err)
	case !fl.Create:
		return nil, newErr(KindNotFound, "open", path, nil)
	}
	return fs.create(ctx, parent, name, path, fl)
}

func (fs *FS) openExisting(ctx context.Context, e *index.Entry, path string, fl OpenFlags) (*Handle, error) {
	switch {
	case fl.Create && fl.Exclusive:
		return nil, newErr(KindExists, "open", path, nil)
	case fl.Directory && !e.IsDir():
		return nil, newErr(KindNotDir, "open", path, nil)
	case fl.NonDirectory && e.IsDir():
		return nil, newErr(KindIsDir, "open", path, nil)
	case e.IsDir() && fl.Truncate:
		return nil, newErr(KindIsDir, "open", path, nil)
	case !e.IsDir() && (fl.Access&AccessWrite != 0 || fl.Truncate) && !canModify(e):
		return nil, newErr(KindAccessDenied, "open", path, nil)
	}
	h, err := fs.attach(e, path, fl.Access, fl.Share)
	if err != nil {
		return nil, err
	}
	if fl.Truncate {
		if err := h.reset(ctx, fl.Attrs); err != nil {
			_ = h.Close()
			return nil, err
		}
	}
	return h, nil
}

func (fs *FS) create(ctx context.Context, parent *index.Entry, name, path string, fl OpenFlags) (*Handle, error) {
	if !canCreateIn(parent) {
		return nil, newErr(KindAccessDenied, "create", path, nil)
	}
	now := time.Now().UTC()
	e := &index.Entry{ID: newLocalID("l-"), ParentID: parent.ID, Name: name, Kind: index.KindFile,
		MTime: now, Created: now, CanWrite: true, CanUpload: true, CanDelete: true,
		LocalOnly: parent.LocalOnly || IsJunk(name), Attrs: fl.Attrs}
	var content *cache.Handle
	if fl.Directory {
		e.Kind = index.KindFolder
	} else {
		key := newLocalID("local-")
		ch, err := fs.cache.CreateLocal(key, "", 0)
		if err != nil {
			return nil, classify("create", path, err)
		}
		content, e.ContentKey = ch, key
	}
	fail := func(err error) (*Handle, error) {
		if content != nil {
			_ = content.Close()
			_ = fs.cache.Remove(e.ContentKey)
		}
		return nil, err
	}
	fs.treeMu.Lock()
	if e.IsDir() && !e.LocalOnly {
		if err := fs.queue(journal.Op{Kind: journal.OpMkdir, NodeID: e.ID, ParentID: parent.ID, Name: name, IsDir: true, MTime: now}); err != nil {
			fs.treeMu.Unlock()
			return fail(err)
		}
	}
	err := fs.idx.Put(e)
	fs.treeMu.Unlock()
	if errors.Is(err, index.ErrNameTaken) {
		return fail(newErr(KindExists, "create", path, nil))
	}
	if err != nil {
		return fail(newErr(KindOther, "create", path, err))
	}
	h, err := fs.attach(e, path, fl.Access, fl.Share)
	if err != nil {
		return fail(err)
	}
	if content != nil {
		on := h.node
		on.mu.Lock()
		on.content, on.written = content, true
		on.mu.Unlock()
	}
	if e.IsDir() && !e.LocalOnly {
		h.mu.Lock()
		h.unsynced = true
		h.mu.Unlock()
	}
	fs.kickNow()
	return h, nil
}

// queue writes an op to the journal (not yet synced: see durable) and
// counts it as pending. The count goes up before the op is visible to the
// dispatcher, so a fast op cannot finish before it was counted.
func (fs *FS) queue(op journal.Op) error {
	_, err := fs.queueSeq(op)
	return err
}

// queueSeq is queue, returning the op's journal sequence number.
func (fs *FS) queueSeq(op journal.Op) (uint64, error) {
	fs.mu.Lock()
	fs.pending[op.NodeID]++
	fs.mu.Unlock()
	written, err := fs.jr.Write(op)
	if err != nil {
		fs.doneLocked(op)
		return 0, newErr(KindOther, "queue", op.Name, err)
	}
	return written.Seq, nil
}

// durable waits until every change queued so far is on the device. Many
// callers at once share one fsync (the journal's group commit).
func (fs *FS) durable(ctx context.Context, what, path string) error {
	if err := fs.jr.Sync(ctx); err != nil {
		return newErr(KindOther, what, path, err)
	}
	return nil
}

// Info returns the open file's current state.
func (h *Handle) Info() Info {
	e := h.node.snapshot()
	return h.fs.info(&e)
}

// ReadDir lists an open folder (see FS.ReadDir).
func (h *Handle) ReadDir(ctx context.Context, marker string, limit int) ([]Info, string, error) {
	if !h.isDir {
		return nil, "", newErr(KindNotDir, "list", h.path, nil)
	}
	e, err := h.fs.idx.Get(h.node.id)
	if err != nil {
		return nil, "", newErr(KindNotFound, "list", h.path, err)
	}
	return h.fs.readDir(ctx, e, marker, limit)
}

// ReadAt reads file bytes, fetching what the cache lacks. It returns io.EOF
// at the end of the file.
func (h *Handle) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if h.isDir {
		return 0, newErr(KindIsDir, "read", h.path, nil)
	}
	if off < 0 {
		return 0, newErr(KindInvalid, "read", h.path, nil)
	}
	on := h.node
	on.mu.Lock()
	e := on.e
	src := on.content
	on.mu.Unlock()
	clean := src == nil
	if clean {
		var err error
		if src, err = h.cleanContent(); err != nil {
			return 0, err
		}
	}
	size := src.Size()
	if off >= size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), size-off)
	ahead := want
	if off == h.lastEnd && clean {
		ahead = min(want+h.fs.cfg.ReadAhead, size-off)
	}
	if miss := src.Missing(off, ahead); len(miss) > 0 {
		// the read-ahead is a bonus; only the bytes asked for must arrive
		if err := h.fs.fetch(ctx, on, src, &e, miss, src.Missing(off, want)); err != nil {
			return 0, err
		}
	}
	n, err := src.ReadAt(p[:want], off)
	h.lastEnd = off + int64(n)
	if n > 0 && clean {
		src.NoteRead() // an application read it: keep it a while (release rules)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, classify("read", h.path, err)
	}
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

// cleanContent opens the cache entry of the drive version for reading.
func (h *Handle) cleanContent() (*cache.Handle, error) {
	on := h.node
	on.mu.Lock()
	defer on.mu.Unlock()
	if on.content != nil {
		return on.content, nil
	}
	want := on.e.VersionID
	if on.e.ContentKey != "" {
		want = on.e.ContentKey // unsent local content
	}
	if on.clean != nil && on.clean.Key() == want {
		return on.clean, nil
	}
	var ch *cache.Handle
	var err error
	switch {
	case on.e.ContentKey != "":
		ch, err = h.fs.cache.Open(on.e.ContentKey)
		if errors.Is(err, cache.ErrNotFound) && on.e.VersionID == "" {
			// sent (and perhaps cleared) since this node was opened: read the
			// drive version the index now names
			if cur, gerr := h.fs.idx.Get(on.e.ID); gerr == nil && cur.ContentKey == "" && cur.VersionID != "" {
				on.e.ServerID, on.e.VersionID, on.e.ContentKey, on.e.Size = cur.ServerID, cur.VersionID, "", cur.Size
			}
		}
		if errors.Is(err, cache.ErrNotFound) && on.e.VersionID != "" {
			ch, err = h.fs.cache.Get(on.e.VersionID, on.e.Size)
		}
	case on.e.VersionID == "":
		return nil, newErr(KindOther, "read", h.path, errors.New("file has no content version"))
	default:
		ch, err = h.fs.cache.Get(on.e.VersionID, on.e.Size)
	}
	if err != nil {
		return nil, classify("read", h.path, err)
	}
	if on.clean != nil {
		_ = on.clean.Close()
	}
	on.clean = ch
	return ch, nil
}

// fetch downloads missing ranges of the drive version into dst. Ranges in
// must are required; the rest (read-ahead) is best effort.
func (fs *FS) fetch(ctx context.Context, on *openNode, dst *cache.Handle, e *index.Entry, ranges, must []cache.Range) error {
	if e.ServerID == "" || e.VersionID == "" {
		return newErr(KindOther, "read", e.Name, errors.New("no drive copy to read from"))
	}
	if fs.offline() {
		return newErr(KindOffline, "read", e.Name, nil)
	}
	on.fetchMu.Lock()
	defer on.fetchMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, fs.cfg.ReadTimeout)
	defer cancel()
	for _, r := range ranges {
		if len(dst.Missing(r.Off, r.Len)) == 0 {
			continue // fetched by another reader meanwhile
		}
		buf := &transfer.BufferAt{Buf: make([]byte, r.Len), Base: r.Off}
		err := fs.eng.Download(ctx, transfer.DownloadRequest{TransferID: "read:" + e.ID, Name: e.Name,
			NodeID: e.ServerID, VersionID: e.VersionID, Offset: r.Off, Length: r.Len, Dst: buf})
		if err != nil {
			fs.noteErr(err)
			if !overlaps(r, must) {
				continue // read-ahead only
			}
			return classify("read", e.Name, err)
		}
		fs.noteOK()
		if err := dst.Fill(r.Off, buf.Buf); err != nil {
			return classify("read", e.Name, err)
		}
	}
	return nil
}

func overlaps(r cache.Range, rs []cache.Range) bool {
	for _, m := range rs {
		if r.Off < m.Off+m.Len && m.Off < r.Off+r.Len {
			return true
		}
	}
	return false
}

// writable returns the local content to write to, making it on first write
// (a copy of the drive version, fetched on demand), or a fresh copy when the
// current local content is being uploaded.
func (h *Handle) writable(ctx context.Context) (*cache.Handle, error) {
	if h.isDir {
		return nil, newErr(KindIsDir, "write", h.path, nil)
	}
	fs, on := h.fs, h.node
	on.mu.Lock()
	defer on.mu.Unlock()
	fs.upMu.Lock()
	busy := on.content != nil && fs.uploading[on.id] == on.content.Key()
	fs.upMu.Unlock()
	if on.content != nil && !busy {
		return on.content, nil
	}
	if !canModify(&on.e) {
		return nil, newErr(KindAccessDenied, "write", h.path, nil)
	}
	base, baseSize := on.e.VersionID, on.e.Size
	if on.content != nil {
		base, baseSize = on.content.Key(), on.content.Size()
	} else if on.e.ContentKey != "" {
		base = on.e.ContentKey
	}
	key := newLocalID("local-")
	ch, err := fs.cache.CreateLocal(key, base, baseSize)
	if err != nil {
		return nil, classify("write", h.path, err)
	}
	if on.content != nil {
		_ = on.content.Close()
	}
	on.content, on.e.ContentKey = ch, key
	return ch, nil
}

// reset empties the file (open with overwrite/supersede).
func (h *Handle) reset(_ context.Context, attrs uint32) error {
	fs, on := h.fs, h.node
	on.mu.Lock()
	defer on.mu.Unlock()
	if !canModify(&on.e) {
		return newErr(KindAccessDenied, "overwrite", h.path, nil)
	}
	key := newLocalID("local-")
	ch, err := fs.cache.CreateLocal(key, "", 0)
	if err != nil {
		return classify("overwrite", h.path, err)
	}
	if on.content != nil {
		old := on.content.Key()
		_ = on.content.Close()
		fs.upMu.Lock()
		uploading := fs.uploading[on.id] == old
		fs.upMu.Unlock()
		if !uploading && old != on.e.ContentKey {
			_ = fs.cache.Remove(old)
		}
	}
	on.content, on.e.ContentKey, on.written = ch, key, true
	on.e.Size, on.e.MTime = 0, time.Now().UTC()
	if attrs != 0 {
		on.e.Attrs, on.attrsSet = attrs, true
	}
	return nil
}

// WriteAt writes at off. Writes land in the cache at local speed; when
// unsent data fills the cache they wait for uploads (back-pressure).
func (h *Handle) WriteAt(ctx context.Context, p []byte, off int64) (int, error) {
	if h.access&AccessWrite == 0 {
		return 0, newErr(KindAccessDenied, "write", h.path, nil)
	}
	if off < 0 {
		return 0, newErr(KindInvalid, "write", h.path, nil)
	}
	ch, err := h.writable(ctx)
	if err != nil {
		return 0, err
	}
	if err := h.fs.cache.WaitForRoom(ctx, int64(len(p))); err != nil {
		return 0, classify("write", h.path, err)
	}
	on := h.node
	if miss := ch.MissingForWrite(off, int64(len(p))); len(miss) > 0 {
		e := on.snapshot()
		if err := h.fs.fetch(ctx, on, ch, &e, miss, miss); err != nil {
			return 0, err
		}
	}
	n, err := ch.WriteAt(p, off)
	on.mu.Lock()
	on.written = true
	on.e.Size = ch.Size()
	if !on.timesSet {
		on.e.MTime = time.Now().UTC()
	}
	on.mu.Unlock()
	if err != nil {
		return n, classify("write", h.path, err)
	}
	return n, nil
}

// Truncate sets the file size.
func (h *Handle) Truncate(ctx context.Context, size int64) error {
	if size < 0 {
		return newErr(KindInvalid, "truncate", h.path, nil)
	}
	on := h.node
	if cur := on.snapshot(); cur.Size == size {
		return nil
	}
	ch, err := h.writable(ctx)
	if err != nil {
		return err
	}
	if err := ch.Truncate(size); err != nil {
		return classify("truncate", h.path, err)
	}
	on.mu.Lock()
	on.written = true
	on.e.Size = size
	if !on.timesSet {
		on.e.MTime = time.Now().UTC()
	}
	on.mu.Unlock()
	return nil
}

// SetTimes sets the modified and created times (zero leaves one alone).
// The modified time is kept on the drive with millisecond precision.
func (h *Handle) SetTimes(_ context.Context, mtime, ctime time.Time) error {
	on := h.node
	on.mu.Lock()
	defer on.mu.Unlock()
	if !mtime.IsZero() {
		if !h.isDir && !canModify(&on.e) {
			return newErr(KindAccessDenied, "set times", h.path, nil)
		}
		on.e.MTime = mtime.UTC().Truncate(time.Millisecond)
		on.timesSet = true
	}
	if !ctime.IsZero() {
		on.e.Created = ctime.UTC()
		on.attrsSet = true
	}
	return nil
}

// SetAttrs keeps OS attributes (hidden, …) on this PC; they are not sent.
func (h *Handle) SetAttrs(_ context.Context, attrs uint32) error {
	on := h.node
	on.mu.Lock()
	defer on.mu.Unlock()
	on.e.Attrs, on.attrsSet = attrs, true
	return nil
}

// Flush makes written data durable on this PC.
func (h *Handle) Flush(context.Context) error {
	on := h.node
	on.mu.Lock()
	ch := on.content
	on.mu.Unlock()
	if ch == nil {
		return nil
	}
	return classify("flush", h.path, ch.Sync())
}

// CanDelete reports whether the file or folder may be deleted now.
func (h *Handle) CanDelete(ctx context.Context) error {
	e := h.node.snapshot()
	return h.fs.canDelete(ctx, &e, h.path)
}

func (fs *FS) canDelete(ctx context.Context, e *index.Entry, path string) error {
	if e.ID == fs.cfg.RootID {
		return newErr(KindAccessDenied, "delete", path, nil)
	}
	if !canRemove(e) {
		return newErr(KindAccessDenied, "delete", path, nil)
	}
	if e.IsDir() {
		if err := fs.ensureListed(ctx, e); err != nil {
			return err
		}
		kids, _, err := fs.idx.List(e.ID, "", 1)
		if err != nil {
			return newErr(KindOther, "delete", path, err)
		}
		if len(kids) > 0 {
			return newErr(KindNotEmpty, "delete", path, nil)
		}
	}
	return nil
}

// SetDeleteOnClose marks the file or folder for deletion when the last
// handle cleans up.
func (h *Handle) SetDeleteOnClose(ctx context.Context, on bool) error {
	if on {
		if err := h.CanDelete(ctx); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.deleteOnClose = on
	h.mu.Unlock()
	return nil
}

// Cleanup is the app closing its handle: a written file is queued for upload
// (when this was the last open), or the file is deleted when delete is set
// or was asked for. The handle may still be read until Close.
func (h *Handle) Cleanup(ctx context.Context, del bool) error {
	h.mu.Lock()
	if h.cleaned {
		h.mu.Unlock()
		return nil
	}
	h.cleaned = true
	del = del || h.deleteOnClose
	h.mu.Unlock()
	fs, on := h.fs, h.node
	fs.mu.Lock()
	last := true
	for o := range on.handles {
		if o != h && !o.isCleaned() {
			last = false
		}
	}
	fs.mu.Unlock()
	on.mu.Lock()
	if del {
		on.doomed = true
	}
	doomed := on.doomed
	on.mu.Unlock()
	if !last {
		return nil
	}
	h.mu.Lock()
	h.unsynced = true
	h.mu.Unlock()
	if doomed {
		return fs.removeEntry(ctx, on.id, h.path)
	}
	return fs.commit(on, h.path)
}

// commit writes an open file's local changes to the index and queues them.
func (fs *FS) commit(on *openNode, path string) error {
	on.mu.Lock()
	e := on.e
	content, written, timesSet, attrsSet := on.content, on.written, on.timesSet, on.attrsSet
	on.mu.Unlock()
	if !written && !timesSet && !attrsSet {
		return nil
	}
	var crc uint32
	if written && content != nil {
		// Like a local disk, closing does not force a new file's data to
		// the device: a crash of this process loses nothing (the OS has the
		// data, and the journal record written below), the cache flush loop
		// makes the data durable within seconds, and an app's own flush
		// (FlushFileBuffers) syncs at once. What would otherwise cost an
		// fsync per file:
		//   - an edit of a drive file needs its block bitmap: synced now;
		//   - a large new file: its data is synced now (one fsync is small
		//     next to writing it);
		//   - any other new file: its CRC goes into the journal record. If
		//     power is lost before its data reached the disk, recovery finds
		//     the CRC does not match and holds the file instead of sending
		//     it damaged.
		switch {
		case !content.Adoptable():
			if err := content.Sync(); err != nil {
				return classify("save", path, err)
			}
		case content.Size() > smallFileMax:
			if err := content.SyncData(); err != nil {
				return classify("save", path, err)
			}
		default:
			sum, err := contentCRC(content)
			if err != nil {
				return classify("save", path, err)
			}
			crc = sum
		}
		e.Size = content.Size()
	}
	fs.treeMu.Lock()
	cur, err := fs.idx.Get(on.id)
	if err != nil {
		fs.treeMu.Unlock()
		return nil // deleted meanwhile
	}
	cur.MTime, cur.Created, cur.Attrs = e.MTime, e.Created, e.Attrs
	if written {
		cur.Size, cur.ContentKey = e.Size, e.ContentKey
	}
	if !cur.LocalOnly {
		switch {
		case written:
			err = fs.queue(journal.Op{Kind: journal.OpUpload, NodeID: cur.ID, ParentID: cur.ParentID, Name: cur.Name,
				ContentKey: cur.ContentKey, Size: cur.Size, MTime: cur.MTime, CRC: crc})
		case timesSet:
			err = fs.queue(journal.Op{Kind: journal.OpSetTime, NodeID: cur.ID, ParentID: cur.ParentID, Name: cur.Name, MTime: cur.MTime})
		}
	}
	if err == nil {
		err = fs.idx.Put(cur)
	}
	fs.treeMu.Unlock()
	if err != nil {
		return classify("save", path, err)
	}
	on.mu.Lock()
	on.written, on.timesSet, on.attrsSet = false, false, false
	on.mu.Unlock()
	fs.kickNow()
	return nil
}

// Close releases the handle. When the handle's Cleanup queued changes,
// Close returns only once their journal records are on the device (the
// journal's group commit: the Closes of many files share one fsync). WinFsp
// sends Close after the app's CloseHandle has returned, so the app does not
// wait for it.
func (h *Handle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	if !h.isCleaned() {
		_ = h.Cleanup(context.Background(), false)
	}
	h.mu.Lock()
	unsynced := h.unsynced
	h.mu.Unlock()
	var syncErr error
	if unsynced {
		syncErr = h.fs.durable(context.Background(), "close", h.path)
	}
	fs, on := h.fs, h.node
	fs.mu.Lock()
	delete(on.handles, h)
	last := len(on.handles) == 0
	if last && fs.open[on.id] == on {
		delete(fs.open, on.id)
	}
	fs.mu.Unlock()
	if last {
		on.mu.Lock()
		if on.content != nil {
			_ = on.content.Close()
			on.content = nil
		}
		if on.clean != nil {
			_ = on.clean.Close()
			on.clean = nil
		}
		on.mu.Unlock()
	}
	return syncErr
}

// Remove implements FileSystem (unlink / rmdir).
func (fs *FS) Remove(ctx context.Context, path string) error {
	e, err := fs.resolve(ctx, path)
	if err != nil {
		return err
	}
	if err := fs.canDelete(ctx, e, path); err != nil {
		return err
	}
	fs.mu.Lock()
	on := fs.open[e.ID]
	fs.mu.Unlock()
	if on != nil {
		on.mu.Lock()
		on.doomed = true
		on.mu.Unlock()
		return nil // deleted when the last handle cleans up
	}
	if err := fs.removeEntry(ctx, e.ID, path); err != nil {
		return err
	}
	return fs.durable(ctx, "delete", path)
}

// removeEntry deletes a node and its subtree locally and queues the drive
// delete (to the trash). Unsent changes inside are dropped: the person
// deleted them.
func (fs *FS) removeEntry(_ context.Context, id, path string) error {
	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()
	e, err := fs.idx.Get(id)
	if errors.Is(err, index.ErrNotFound) {
		return nil
	}
	if err != nil {
		return newErr(KindOther, "delete", path, err)
	}
	if e.ServerID != "" && !e.LocalOnly {
		if err := fs.queue(journal.Op{Kind: journal.OpDelete, NodeID: e.ID, ParentID: e.ParentID, Name: e.Name,
			ServerID: e.ServerID, IsDir: e.IsDir()}); err != nil {
			return err
		}
	}
	var removed []*index.Entry
	err = fs.idx.Update(func(tx *index.Tx) (err error) {
		removed, err = tx.Delete(id)
		return err
	})
	if err != nil {
		return newErr(KindOther, "delete", path, err)
	}
	gone := map[string]bool{}
	for _, r := range removed {
		gone[r.ID] = true
	}
	fs.dropOps(gone, id)
	for _, r := range removed {
		if r.ContentKey != "" {
			_ = fs.cache.Remove(r.ContentKey)
		}
	}
	fs.kickNow()
	return nil
}

// dropOps cancels queued and running changes of deleted nodes (except the
// delete op of keep).
func (fs *FS) dropOps(gone map[string]bool, keep string) {
	ops, err := fs.jr.List()
	if err != nil {
		fs.log.Warn("vfs: read journal", "err", err)
		return
	}
	for _, op := range ops {
		if !gone[op.NodeID] || (op.NodeID == keep && op.Kind == journal.OpDelete) {
			continue
		}
		fs.mu.Lock()
		r := fs.inflight[op.Seq]
		if r != nil {
			r.cancelled = true
			r.cancel()
		}
		fs.mu.Unlock()
		if r == nil {
			if err := fs.jr.Done(op.Seq); err == nil {
				fs.doneLocked(op)
			}
		}
	}
}

func (fs *FS) doneLocked(op journal.Op) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.pending[op.NodeID] > 0 {
		fs.pending[op.NodeID]--
	}
	if fs.pending[op.NodeID] == 0 {
		delete(fs.pending, op.NodeID)
	}
}

// Rename implements FileSystem: rename and/or move, replacing an existing
// file when replace is set.
func (fs *FS) Rename(ctx context.Context, from, to string, replace bool) error {
	src, err := fs.resolve(ctx, from)
	if err != nil {
		return err
	}
	if src.ID == fs.cfg.RootID {
		return newErr(KindAccessDenied, "rename", from, nil)
	}
	parts := splitPath(to)
	if len(parts) == 0 {
		return newErr(KindInvalid, "rename", to, nil)
	}
	dst, err := fs.walk(ctx, parts[:len(parts)-1], to)
	if err != nil {
		return err
	}
	if !dst.IsDir() {
		return newErr(KindNotDir, "rename", to, nil)
	}
	name := DecodeName(parts[len(parts)-1])
	if !validName(name) {
		return newErr(KindInvalidName, "rename", to, nil)
	}
	if !src.LocalOnly && !canModify(src) {
		return newErr(KindAccessDenied, "rename", from, nil)
	}
	if dst.ID != src.ParentID && !canCreateIn(dst) {
		return newErr(KindAccessDenied, "rename", to, nil)
	}
	for cur := dst; ; {
		if cur.ID == src.ID {
			return newErr(KindInvalid, "rename", to, errors.New("a folder cannot move into itself"))
		}
		if cur.ParentID == "" {
			break
		}
		if cur, err = fs.idx.Get(cur.ParentID); err != nil {
			break
		}
	}
	if err := fs.ensureListed(ctx, dst); err != nil {
		return err
	}
	// A file not yet on the drive saved over a drive file (rename over it,
	// or Word's way: the original renamed to a temp name just before) is a
	// new version of that drive file, not a delete and a new file.
	newFile := !src.IsDir() && src.ServerID == "" && !dst.LocalOnly && !IsJunk(name)
	var adopt *index.Entry
	var displacedSeq uint64
	if ex, err := fs.idx.Lookup(dst.ID, name); err == nil && ex.ID != src.ID {
		if !replace {
			return newErr(KindExists, "rename", to, nil)
		}
		if ex.IsDir() {
			return newErr(KindAccessDenied, "rename", to, errors.New("cannot replace a folder"))
		}
		fs.mu.Lock()
		on := fs.open[ex.ID]
		fs.mu.Unlock()
		if on != nil {
			for o := range on.handles {
				if !o.isCleaned() {
					return newErr(KindAccessDenied, "rename", to, errors.New("the file being replaced is open"))
				}
			}
		}
		if newFile && ex.ServerID != "" && !ex.LocalOnly && canModify(ex) {
			adopt = ex
		} else {
			if err := fs.canDelete(ctx, ex, to); err != nil {
				return err
			}
			if err := fs.removeEntry(ctx, ex.ID, to); err != nil {
				return err
			}
		}
	} else if newFile {
		adopt, displacedSeq = fs.takeDisplaced(dst.ID, name)
	}
	if adopt != nil {
		if err := fs.adoptVersion(ctx, src, adopt, displacedSeq, dst, name); err != nil {
			return classify("rename", to, err)
		}
		fs.kickNow()
		return fs.durable(ctx, "rename", to)
	}
	fs.treeMu.Lock()
	cur, err := fs.idx.Get(src.ID)
	if err != nil {
		fs.treeMu.Unlock()
		return newErr(KindNotFound, "rename", from, err)
	}
	wasJunk := cur.LocalOnly
	cur.ParentID, cur.Name = dst.ID, name
	cur.LocalOnly = dst.LocalOnly || (wasJunk && IsJunk(name))
	switch {
	case wasJunk && !cur.LocalOnly:
		// a temp file saved under its real name: it becomes a drive file
		op := journal.Op{Kind: journal.OpUpload, NodeID: cur.ID, ParentID: dst.ID, Name: name, ContentKey: cur.ContentKey, Size: cur.Size, MTime: cur.MTime}
		if cur.IsDir() {
			op = journal.Op{Kind: journal.OpMkdir, NodeID: cur.ID, ParentID: dst.ID, Name: name, IsDir: true, MTime: cur.MTime}
		}
		err = fs.queue(op)
	case !wasJunk:
		op := journal.Op{Kind: journal.OpRename, NodeID: cur.ID, ParentID: dst.ID, Name: name, ServerID: cur.ServerID}
		displaced := cur.ServerID != "" && !cur.IsDir() && isSaveTemp(name) && dst.ID == src.ParentID
		if displaced {
			// Word renames the original to a temp name, then its new copy to
			// the original name: wait a moment so that becomes a new version.
			op.NotBefore = time.Now().Add(displaceHold)
		}
		var seq uint64
		seq, err = fs.queueSeq(op)
		if err == nil && displaced {
			fs.noteDisplaced(src.ParentID, src.Name, cur.ID, seq)
		}
	}
	if err == nil {
		err = fs.idx.Put(cur)
	}
	fs.treeMu.Unlock()
	if errors.Is(err, index.ErrNameTaken) {
		return newErr(KindExists, "rename", to, nil)
	}
	if err != nil {
		return classify("rename", to, err)
	}
	fs.mu.Lock()
	on := fs.open[src.ID]
	fs.mu.Unlock()
	if on != nil {
		on.mu.Lock()
		on.e.ParentID, on.e.Name, on.e.LocalOnly = cur.ParentID, cur.Name, cur.LocalOnly
		on.mu.Unlock()
	}
	fs.kickNow()
	return fs.durable(ctx, "rename", to)
}
