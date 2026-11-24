package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// errObsolete: the op no longer applies (its node is gone); drop it.
var errObsolete = errors.New("vfs: change no longer applies")

// errNotReady: the op waits for another (e.g. its folder to be created).
var errNotReady = errors.New("vfs: change waits for another")

// heldError carries a refusal the drive will not change its mind about.
type heldError struct {
	code, reason string
}

func (e *heldError) Error() string { return "vfs: held: " + e.code }

func (fs *FS) dispatchLoop() {
	for {
		fs.dispatch()
		select {
		case <-fs.ctx.Done():
			return
		case <-fs.kick:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// dispatch starts queued changes in journal order. An op waits while an
// earlier unfinished op touches its node or its folder, or (renames) while
// an earlier delete in the same folder is unfinished, so a rename over a
// deleted file reaches the drive after the delete.
func (fs *FS) dispatch() {
	if fs.State() == StateSignedOut || fs.ctx.Err() != nil {
		return
	}
	ops, err := fs.jr.List()
	if err != nil {
		fs.log.Warn("vfs: read journal", "err", err)
		return
	}
	now := time.Now()
	busyNode := map[string]bool{}
	deletesIn := map[string]bool{}
	for _, op := range ops {
		blocked := busyNode[op.NodeID] || (op.ParentID != "" && busyNode[op.ParentID]) ||
			(op.Kind == journal.OpRename && deletesIn[op.ParentID])
		busyNode[op.NodeID] = true
		if op.Kind == journal.OpDelete {
			deletesIn[op.ParentID] = true
		}
		if blocked || op.State != journal.StatePending || now.Before(op.NotBefore) {
			continue
		}
		fs.mu.Lock()
		_, active := fs.inflight[op.Seq]
		full := len(fs.inflight) >= fs.cfg.Workers
		writing := false
		if on := fs.open[op.NodeID]; on != nil && op.Kind == journal.OpUpload {
			writing = on.hasLocalChanges()
		}
		if active || full || writing {
			fs.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(fs.ctx)
		fs.inflight[op.Seq] = &running{op: op, cancel: cancel}
		fs.mu.Unlock()
		fs.wg.Add(1)
		go func(op journal.Op) {
			defer fs.wg.Done()
			defer cancel()
			fs.run(ctx, op)
		}(op)
	}
}

// run executes one op and records the outcome.
func (fs *FS) run(ctx context.Context, op journal.Op) {
	err := fs.exec(ctx, &op)
	fs.mu.Lock()
	r := fs.inflight[op.Seq]
	delete(fs.inflight, op.Seq)
	cancelled := r != nil && r.cancelled
	fs.mu.Unlock()
	defer fs.kickNow()
	if cancelled {
		if op.UploadID != "" {
			actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = fs.api.AbortUpload(actx, op.UploadID)
			acancel()
		}
		fs.finish(op)
		return
	}
	var held *heldError
	switch {
	case err == nil:
		fs.noteOK()
		fs.finish(op)
		fs.emit(Event{Kind: EventSynced, Seq: op.Seq, Path: fs.pathOf(op.NodeID)})
	case errors.Is(err, errObsolete):
		fs.finish(op)
	case errors.As(err, &held):
		fs.hold(op, held.code, held.reason)
	case driveapi.IsPermanent(err):
		e, _ := driveapi.AsError(err)
		fs.hold(op, e.Code, driveapi.Reason(err))
	case fs.ctx.Err() != nil:
		// stopping: the op stays queued as it is
	default:
		fs.noteErr(err)
		op.Attempts++
		op.LastError = errCode(err)
		wait := fs.cfg.Backoff.Delay(min(op.Attempts, 8), err)
		if errors.Is(err, errNotReady) {
			wait = 200 * time.Millisecond
		}
		op.NotBefore = time.Now().Add(wait)
		if driveapi.IsNameConflict(err) && op.Attempts >= 5 {
			fs.hold(op, driveapi.CodeNameConflict, "Something with this name already exists in the folder on the drive.")
			return
		}
		if uerr := fs.jr.Update(op); uerr != nil {
			fs.log.Warn("vfs: update journal", "err", uerr)
		}
		fs.log.Debug("vfs: change will be retried", "seq", op.Seq, "kind", op.Kind, "code", op.LastError)
	}
}

func errCode(err error) string {
	if e, ok := driveapi.AsError(err); ok {
		if e.Code != "" {
			return e.Code
		}
		if e.Status == 0 {
			return "network"
		}
		return strconv.Itoa(e.Status)
	}
	if errors.Is(err, errNotReady) {
		return "waiting"
	}
	return "error"
}

func (fs *FS) finish(op journal.Op) {
	if err := fs.jr.Done(op.Seq); err != nil {
		fs.log.Warn("vfs: journal done", "err", err)
		return
	}
	fs.doneLocked(op)
}

func (fs *FS) hold(op journal.Op, code, reason string) {
	if reason == "" {
		reason = "The drive refused this change."
	}
	op.State, op.HeldCode, op.Reason = journal.StateHeld, code, reason
	if err := fs.jr.Update(op); err != nil {
		fs.log.Warn("vfs: hold", "err", err)
	}
	fs.log.Info("vfs: change held", "seq", op.Seq, "kind", op.Kind, "code", code)
	fs.emit(Event{Kind: EventHeld, Seq: op.Seq, Path: fs.pathOf(op.NodeID), Reason: reason})
}

func (fs *FS) exec(ctx context.Context, op *journal.Op) error {
	switch op.Kind {
	case journal.OpMkdir:
		return fs.execMkdir(ctx, op)
	case journal.OpUpload:
		return fs.execUpload(ctx, op)
	case journal.OpRename:
		return fs.execRename(ctx, op)
	case journal.OpDelete:
		return fs.execDelete(ctx, op)
	case journal.OpSetTime:
		return fs.execSetTime(ctx, op)
	}
	return fmt.Errorf("vfs: unknown op %q: %w", op.Kind, errObsolete)
}

// entryAndParent reads an op's node and its folder's drive id.
func (fs *FS) entryAndParent(op *journal.Op) (*index.Entry, *index.Entry, error) {
	e, err := fs.idx.Get(op.NodeID)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, errObsolete
	}
	if err != nil {
		return nil, nil, err
	}
	p, err := fs.idx.Get(e.ParentID)
	if errors.Is(err, index.ErrNotFound) {
		return nil, nil, errObsolete
	}
	if err != nil {
		return nil, nil, err
	}
	if p.ServerID == "" {
		return nil, nil, errNotReady
	}
	return e, p, nil
}

func (fs *FS) execMkdir(ctx context.Context, op *journal.Op) error {
	e, p, err := fs.entryAndParent(op)
	if err != nil {
		return err
	}
	if e.ServerID != "" {
		return nil
	}
	n, err := fs.api.CreateFolder(ctx, p.ServerID, e.Name, driveapi.ConflictFail, e.MTime)
	if driveapi.IsNameConflict(err) {
		// replay after a crash, or made elsewhere meanwhile: adopt a folder of
		// that name; beside a file, make ours a conflict copy
		n, err = fs.adoptFolder(ctx, p.ServerID, e.Name)
	}
	if driveapi.IsNotFound(err) {
		return &heldError{code: "folder_gone", reason: "The folder this was made in was deleted on the drive."}
	}
	if err != nil {
		return err
	}
	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()
	return fs.idx.Update(func(tx *index.Tx) error {
		cur, err := tx.Get(e.ID)
		if err != nil {
			return nil
		}
		if dup, err := tx.BySID(n.ID); err == nil && dup.ID != cur.ID {
			if _, err := tx.Delete(dup.ID); err != nil {
				return err
			}
		}
		cur.ServerID = n.ID
		cur.Name = n.Name
		cur.CanWrite, cur.CanUpload, cur.CanDelete, cur.Sealed = n.Can.Write, n.Can.Upload, n.Can.Delete, n.Sealed
		if err := tx.Put(cur); err != nil {
			return err
		}
		return tx.SetListed(cur.ID, true)
	})
}

func (fs *FS) adoptFolder(ctx context.Context, parentSID, name string) (*driveapi.Node, error) {
	kids, err := fs.api.AllChildren(ctx, parentSID)
	if err != nil {
		return nil, err
	}
	for i := range kids {
		if index.Fold(kids[i].Name) == index.Fold(name) && kids[i].IsDir() {
			return &kids[i], nil
		}
	}
	return fs.api.CreateFolder(ctx, parentSID, ConflictName(name, fs.cfg.PCName, time.Now()), driveapi.ConflictRename, time.Time{})
}

func (fs *FS) execUpload(ctx context.Context, op *journal.Op) error {
	e, p, err := fs.entryAndParent(op)
	if err != nil {
		return err
	}
	key := e.ContentKey
	if key == "" {
		return nil // sent by an earlier op, or discarded
	}
	h, err := fs.cache.Open(key)
	if errors.Is(err, cache.ErrNotFound) {
		return &heldError{code: "local_copy_lost", reason: "The copy on this PC was lost before it was sent."}
	}
	if err != nil {
		return err
	}
	defer h.Close()
	fs.upMu.Lock()
	fs.uploading[e.ID] = key
	fs.upMu.Unlock()
	defer func() {
		fs.upMu.Lock()
		if fs.uploading[e.ID] == key {
			delete(fs.uploading, e.ID)
		}
		fs.upMu.Unlock()
	}()
	// an edited drive file: fetch the bytes the edit did not touch
	if miss := h.Missing(0, h.Size()); len(miss) > 0 {
		for _, r := range miss {
			buf := &transfer.BufferAt{Buf: make([]byte, r.Len), Base: r.Off}
			if err := fs.eng.Download(ctx, transfer.DownloadRequest{TransferID: "base:" + e.ID, Name: e.Name,
				NodeID: e.ServerID, VersionID: e.VersionID, Offset: r.Off, Length: r.Len, Dst: buf}); err != nil {
				return err
			}
			if err := h.Fill(r.Off, buf.Buf); err != nil {
				return err
			}
		}
	}
	size := h.Size()
	req := transfer.UploadRequest{TransferID: strconv.FormatUint(op.Seq, 10), ParentID: p.ServerID, Name: e.Name,
		Size: size, Src: h, ModifiedAt: e.MTime, Fingerprint: "olsyn-drive|" + key + "|" + strconv.FormatInt(size, 10),
		UploadID: op.UploadID, Verify: size <= 64<<20, Conflict: driveapi.ConflictFail,
		OnSession: func(id string) {
			if op.UploadID != id {
				op.UploadID = id
				if err := fs.jr.Update(*op); err != nil {
					fs.log.Warn("vfs: record upload session", "err", err)
				}
			}
		}}
	if e.ServerID != "" {
		req.Conflict, req.BaseVersionID = driveapi.ConflictNewVersion, e.VersionID
	}
	res, err := fs.eng.Upload(ctx, req)
	switch {
	case err == nil:
		return fs.finishUpload(ctx, e.ID, key, res.Node)
	case driveapi.IsVersionConflict(err) || (driveapi.IsNameConflict(err) && e.ServerID == ""):
		return fs.conflictCopy(ctx, op, e, p, h)
	case driveapi.IsNotFound(err):
		return &heldError{code: "folder_gone", reason: "The folder this file was saved in was deleted on the drive."}
	}
	return err
}

// finishUpload records a sent file: its cache content becomes the drive
// version's, and the entry learns its drive id and version.
func (fs *FS) finishUpload(ctx context.Context, id, key string, n *driveapi.Node) error {
	if err := fs.cache.Commit(key, n.VersionID()); err != nil && !errors.Is(err, cache.ErrNotFound) {
		fs.log.Warn("vfs: commit cache", "err", err)
	}
	fs.treeMu.Lock()
	gone := false
	err := fs.idx.Update(func(tx *index.Tx) error {
		cur, err := tx.Get(id)
		if errors.Is(err, index.ErrNotFound) {
			gone = true
			return nil
		}
		if err != nil {
			return err
		}
		if dup, err := tx.BySID(n.ID); err == nil && dup.ID != cur.ID {
			if _, err := tx.Delete(dup.ID); err != nil {
				return err
			}
		}
		cur.ServerID, cur.VersionID = n.ID, n.VersionID()
		if n.CurrentVersion != nil {
			cur.SHA256 = n.CurrentVersion.SHA256
		}
		cur.CanWrite, cur.CanUpload, cur.CanDelete, cur.Sealed, cur.Managed = n.Can.Write, n.Can.Upload, n.Can.Delete, n.Sealed, n.IsManaged()
		if cur.ContentKey == key {
			cur.ContentKey, cur.Size = "", n.Size
		}
		return tx.Put(cur)
	})
	fs.treeMu.Unlock()
	if err != nil {
		return err
	}
	if gone {
		// deleted here while it was going up: delete wins
		return fs.api.TrashNode(ctx, n.ID)
	}
	fs.mu.Lock()
	on := fs.open[id]
	fs.mu.Unlock()
	if on != nil {
		on.mu.Lock()
		on.e.ServerID, on.e.VersionID = n.ID, n.VersionID()
		if on.e.ContentKey == key {
			on.e.ContentKey = ""
		}
		on.mu.Unlock()
	}
	return nil
}

// conflictCopy sends our content beside the drive's file as
// "name (conflict from <PC>, <date>).ext"; nothing is overwritten.
func (fs *FS) conflictCopy(ctx context.Context, op *journal.Op, e, p *index.Entry, h *cache.Handle) error {
	name := ConflictName(e.Name, fs.cfg.PCName, time.Now())
	key := e.ContentKey
	res, err := fs.eng.Upload(ctx, transfer.UploadRequest{TransferID: strconv.FormatUint(op.Seq, 10) + "-conflict",
		ParentID: p.ServerID, Name: name, Size: h.Size(), Src: h, ModifiedAt: e.MTime, Conflict: driveapi.ConflictRename,
		Fingerprint: "olsyn-drive|" + key + "|conflict"})
	if err != nil {
		return err
	}
	var fresh *driveapi.Node
	if e.ServerID != "" {
		nw, err := fs.api.GetNode(ctx, e.ServerID)
		if err == nil {
			fresh = &nw.Node
		}
	}
	if err := fs.cache.Commit(key, res.Node.VersionID()); err != nil && !errors.Is(err, cache.ErrNotFound) {
		fs.log.Warn("vfs: commit cache", "err", err)
	}
	fs.treeMu.Lock()
	err = fs.idx.Update(func(tx *index.Tx) error {
		cur, err := tx.Get(e.ID)
		if err != nil {
			return nil
		}
		if cur.ServerID == "" {
			// our new file met a drive file of the same name: ours is the copy
			cur.ServerID, cur.Name, cur.VersionID = res.Node.ID, res.Node.Name, res.Node.VersionID()
			cur.ContentKey, cur.Size = "", res.Node.Size
			if err := tx.Put(cur); err != nil {
				return err
			}
			return tx.SetListed(p.ID, false) // bring the drive's file in
		}
		// the drive's file changed since we opened it: it stays as it is,
		// ours goes beside it
		if cur.ContentKey == key {
			cur.ContentKey = ""
			if fresh != nil {
				cur.Size, cur.VersionID, cur.MTime = fresh.Size, fresh.VersionID(), fresh.MTime()
			} else {
				cur.VersionID = ""
			}
			if err := tx.Put(cur); err != nil {
				return err
			}
		}
		return fs.mergeNode(tx, res.Node, p.ID)
	})
	fs.treeMu.Unlock()
	if err != nil {
		return err
	}
	fs.relist(p.ID)
	fs.log.Info("vfs: conflict copy made", "seq", op.Seq)
	fs.emit(Event{Kind: EventConflict, Seq: op.Seq, Path: fs.pathOf(p.ID) + "/" + EncodeName(res.Node.Name),
		Reason: "Someone else changed this file on the drive; your version was saved beside it."})
	return nil
}

func (fs *FS) execRename(ctx context.Context, op *journal.Op) error {
	e, p, err := fs.entryAndParent(op)
	if err != nil {
		return err
	}
	if e.ServerID == "" {
		return nil // not on the drive yet: it is created with its current name
	}
	name := e.Name
	_, err = fs.api.PatchNode(ctx, e.ServerID, driveapi.Patch{Name: &name, ParentID: &p.ServerID})
	if driveapi.IsNotFound(err) {
		return errObsolete
	}
	return err
}

func (fs *FS) execDelete(ctx context.Context, op *journal.Op) error {
	if op.ServerID == "" {
		return nil
	}
	err := fs.api.TrashNode(ctx, op.ServerID)
	if driveapi.IsNotFound(err) {
		return nil
	}
	return err
}

func (fs *FS) execSetTime(ctx context.Context, op *journal.Op) error {
	e, err := fs.idx.Get(op.NodeID)
	if err != nil {
		return errObsolete
	}
	if e.ServerID == "" {
		return nil // the upload carries it
	}
	mt := e.MTime
	_, err = fs.api.PatchNode(ctx, e.ServerID, driveapi.Patch{ModifiedAt: &mt})
	if driveapi.IsNotFound(err) {
		return errObsolete
	}
	return err
}

// ---- queue ----

// Queued lists changes not yet on the drive and not held, in queue order.
func (fs *FS) Queued() []Queued {
	ops, err := fs.jr.List()
	if err != nil {
		return nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]Queued, 0, len(ops))
	for _, op := range ops {
		if op.State != journal.StatePending {
			continue
		}
		_, active := fs.inflight[op.Seq]
		out = append(out, Queued{Seq: op.Seq, NodeID: op.NodeID, Kind: op.Kind, Name: op.Name, Size: op.Size,
			QueuedAt: op.Queued, Active: active, Attempts: op.Attempts, LastError: op.LastError, NotBefore: op.NotBefore})
	}
	return out
}

// PathOf is the drive path ("/a/b.txt", OS names) of a local id, or "" when
// it is gone.
func (fs *FS) PathOf(id string) string { return fs.pathOf(id) }

// OpenFiles counts files (not folders) open on the drive right now.
func (fs *FS) OpenFiles() int {
	fs.mu.Lock()
	nodes := make([]*openNode, 0, len(fs.open))
	for _, on := range fs.open {
		nodes = append(nodes, on)
	}
	fs.mu.Unlock()
	n := 0
	for _, on := range nodes {
		on.mu.Lock()
		if !on.e.IsDir() && len(on.handles) > 0 {
			n++
		}
		on.mu.Unlock()
	}
	return n
}

// ---- held changes ----

// Held lists changes the drive refused, oldest first.
func (fs *FS) Held() []Held {
	ops, err := fs.jr.List()
	if err != nil {
		return nil
	}
	var out []Held
	for _, op := range ops {
		if op.State != journal.StateHeld {
			continue
		}
		p := fs.pathOf(op.NodeID)
		if p == "" {
			p = EncodeName(op.Name)
		}
		out = append(out, Held{Seq: op.Seq, Path: p, Kind: op.Kind, Code: op.HeldCode, Reason: op.Reason, Size: op.Size, Since: op.Queued})
	}
	return out
}

// Discard drops a held change and puts the drive's state back where the
// change had replaced it (a refused new file disappears; a refused edit
// shows the drive's version again; a refused delete or rename is undone).
func (fs *FS) Discard(ctx context.Context, seq uint64) error {
	op, err := fs.jr.Get(seq)
	if err != nil {
		return newErr(KindNotFound, "discard", "", err)
	}
	if op.State != journal.StateHeld {
		return newErr(KindInvalid, "discard", "", errors.New("only held changes can be discarded"))
	}
	fs.finish(op)
	e, err := fs.idx.Get(op.NodeID)
	switch {
	case op.Kind == journal.OpDelete:
		if p, perr := fs.idx.Get(op.ParentID); perr == nil {
			fs.treeMu.Lock()
			_ = fs.idx.Update(func(tx *index.Tx) error { return tx.SetListed(p.ID, false) })
			fs.treeMu.Unlock()
		}
	case err != nil:
	case e.ServerID == "":
		// never reached the drive: remove it here
		if rerr := fs.removeEntry(ctx, e.ID, ""); rerr != nil {
			return rerr
		}
	default:
		if op.Kind == journal.OpUpload && e.ContentKey == op.ContentKey {
			_ = fs.cache.Remove(e.ContentKey)
		}
		fs.treeMu.Lock()
		_ = fs.idx.Update(func(tx *index.Tx) error {
			cur, err := tx.Get(e.ID)
			if err != nil {
				return nil
			}
			if op.Kind == journal.OpUpload && cur.ContentKey == op.ContentKey {
				cur.ContentKey = ""
			}
			if err := tx.Put(cur); err != nil {
				return err
			}
			return tx.SetListed(cur.ParentID, false)
		})
		fs.treeMu.Unlock()
		if nw, gerr := fs.api.GetNode(ctx, e.ServerID); gerr == nil {
			if p, perr := fs.idx.BySID(nw.Node.ParentID); perr == nil {
				fs.treeMu.Lock()
				_ = fs.idx.Update(func(tx *index.Tx) error { return fs.mergeNode(tx, &nw.Node, p.ID) })
				fs.treeMu.Unlock()
			}
		}
	}
	fs.emit(Event{Kind: EventReleased, Seq: seq, Path: EncodeName(op.Name), Reason: "discarded"})
	fs.kickNow()
	return nil
}

// SaveCopy writes a held file's local content into dir (a folder on this
// PC) and returns the path written. The held change stays until Discard.
func (fs *FS) SaveCopy(ctx context.Context, seq uint64, dir string) (string, error) {
	op, err := fs.jr.Get(seq)
	if err != nil {
		return "", newErr(KindNotFound, "save copy", "", err)
	}
	key := op.ContentKey
	if e, err := fs.idx.Get(op.NodeID); err == nil && e.ContentKey != "" {
		key = e.ContentKey
	}
	if op.Kind != journal.OpUpload || key == "" {
		return "", newErr(KindInvalid, "save copy", "", errors.New("only held files have content to save"))
	}
	h, err := fs.cache.Open(key)
	if err != nil {
		return "", newErr(KindNotFound, "save copy", "", err)
	}
	defer h.Close()
	name := EncodeName(op.Name)
	if e, err := fs.idx.Get(op.NodeID); err == nil {
		name = EncodeName(e.Name)
	}
	target := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	for i := 2; ; i++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		}
		target = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", name[:len(name)-len(ext)], i, ext))
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", newErr(KindOther, "save copy", target, err)
	}
	if _, err := io.Copy(f, io.NewSectionReader(h, 0, h.Size())); err != nil {
		_ = f.Close()
		_ = os.Remove(target)
		return "", newErr(KindOther, "save copy", target, err)
	}
	if err := f.Close(); err != nil {
		return "", newErr(KindOther, "save copy", target, err)
	}
	if !op.MTime.IsZero() {
		_ = os.Chtimes(target, op.MTime, op.MTime)
	}
	_ = ctx
	return target, nil
}

// Pending is how many changes are queued (including held ones).
//
// It counts from the file system's own bookkeeping, which is updated after
// the journal: when Pending is 0, every change is on the drive and every
// Stat already says so.
func (fs *FS) Pending() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	n := 0
	for _, c := range fs.pending {
		n += c
	}
	return n
}
