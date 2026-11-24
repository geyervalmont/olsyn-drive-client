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

// errSentAfterDelete: an upload finished on the drive after its file was
// deleted here; what it made must go to the trash (see discardUpload).
var errSentAfterDelete = errors.New("vfs: file deleted here while it was being sent")

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
// earlier unfinished op touches its node or its folder; a rename waits for
// an earlier unfinished delete in the same folder, and a new file or folder
// or a rename waits for an earlier delete of the same name there, or an
// earlier rename away from it, so a save over a deleted file (Office's save
// by rename) reaches the drive after the delete and a name is free on the
// drive before it is taken again.
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
	freedName := map[string]bool{} // parent id + NUL + folded name, deleted or renamed away
	live := make(map[uint64]bool, len(ops))
	for _, op := range ops {
		live[op.Seq] = true
	}
	waiting := func(op journal.Op) bool {
		for _, s := range op.WaitFor {
			if s < op.Seq && live[s] {
				return true
			}
		}
		return false
	}
	nameKey := func(parent, name string) string { return parent + "\x00" + index.Fold(name) }
	for _, op := range ops {
		takesName := op.Kind == journal.OpUpload || op.Kind == journal.OpMkdir || op.Kind == journal.OpRename
		blocked := busyNode[op.NodeID] || (op.ParentID != "" && busyNode[op.ParentID]) ||
			(op.Kind == journal.OpRename && deletesIn[op.ParentID]) ||
			(takesName && freedName[nameKey(op.ParentID, op.Name)]) || waiting(op)
		busyNode[op.NodeID] = true
		switch op.Kind {
		case journal.OpDelete:
			deletesIn[op.ParentID] = true
			freedName[nameKey(op.ParentID, op.Name)] = true
		case journal.OpRename:
			if op.FromParent != "" {
				freedName[nameKey(op.FromParent, op.FromName)] = true
			}
		}
		if blocked || op.State != journal.StatePending || now.Before(op.NotBefore) {
			continue
		}
		fs.mu.Lock()
		_, active := fs.inflight[op.Seq]
		if !active {
			// the list above may predate this op's run finishing (it
			// leaves inflight only once its outcome is in the journal):
			// start what the journal holds now, if anything
			cur, err := fs.jr.Get(op.Seq)
			if err != nil || cur.State != journal.StatePending || now.Before(cur.NotBefore) {
				fs.mu.Unlock()
				continue
			}
			op = cur
		}
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
//
// The op stays in inflight until its outcome is in the journal: a dispatch
// pass in between would otherwise see it queued and not running, and run
// it a second time (a file sent twice, the second time as a new version
// under a name it was renamed to meanwhile).
func (fs *FS) run(ctx context.Context, op journal.Op) {
	err := fs.exec(ctx, &op)
	defer fs.kickNow()
	defer func() {
		fs.mu.Lock()
		delete(fs.inflight, op.Seq)
		fs.mu.Unlock()
	}()
	fs.mu.Lock()
	r := fs.inflight[op.Seq]
	cancelled := r != nil && r.cancelled
	fs.mu.Unlock()
	if cancelled || errors.Is(err, errSentAfterDelete) {
		// the node was deleted here (or replaced): an upload it started
		// must leave nothing on the drive before the op may go
		if op.Kind == journal.OpUpload && op.UploadID != "" {
			cctx, ccancel := context.WithTimeout(fs.ctx, 30*time.Second)
			derr := fs.discardUpload(cctx, op.UploadID)
			ccancel()
			if derr != nil {
				fs.retryLater(op, derr)
				return
			}
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
		if e, gerr := fs.idx.Get(op.NodeID); gerr == nil && op.Kind != journal.OpUpload && op.Kind != journal.OpMkdir {
			// the drive no longer has what this change was for, but the
			// node is still here: look again so the two agree
			fs.log.Warn("vfs: change no longer applies on the drive", "seq", op.Seq, "kind", op.Kind)
			fs.relist(e.ParentID)
			fs.wakeNow()
		}
	case errors.As(err, &held):
		fs.hold(op, held.code, held.reason)
	case errors.Is(err, transfer.ErrChecksumMismatch):
		fs.hold(op, driveapi.CodeChecksumMismatch, driveapi.ChecksumMismatchReason)
	case driveapi.IsPermanent(err):
		e, _ := driveapi.AsError(err)
		fs.hold(op, e.Code, driveapi.Reason(err))
	case fs.ctx.Err() != nil:
		// stopping: the op stays queued as it is
	default:
		fs.retryLater(op, err)
	}
}

// retryLater keeps an op queued to run again after a backoff.
func (fs *FS) retryLater(op journal.Op, err error) {
	if fs.ctx.Err() != nil {
		return // stopping: the op stays queued as it is
	}
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

// discardUpload makes sure an upload session of a file deleted here leaves
// nothing on the drive. Aborting an open session is enough: it can no
// longer complete. A session the drive no longer has either never finished
// or finished just before (the answer lost, or still on its way when the
// delete came): completing it again is safe and names the file it made,
// which then goes to the trash unless a file here is that drive file.
func (fs *FS) discardUpload(ctx context.Context, uploadID string) error {
	err := fs.api.AbortUpload(ctx, uploadID)
	if err == nil {
		return nil
	}
	if !driveapi.IsNotFound(err) && !driveapi.IsUploadGone(err) {
		return fmt.Errorf("vfs: abort upload: %w", err)
	}
	n, err := fs.api.Complete(ctx, uploadID, driveapi.CompleteUpload{})
	if driveapi.IsNotFound(err) || driveapi.IsUploadGone(err) {
		return nil // aborted, or reaped unfinished
	}
	if err != nil {
		return fmt.Errorf("vfs: look up finished upload: %w", err)
	}
	return fs.trashMade(ctx, n.ID)
}

// trashMade sends a node this PC made on the drive, for a file or folder
// deleted here meanwhile, to the trash, unless a file made here is that
// node (saved over it). An entry the drive's announcement of the node
// brought in meanwhile is removed with it.
func (fs *FS) trashMade(ctx context.Context, sid string) error {
	if e, err := fs.idx.BySID(sid); err == nil {
		if e.ID != sid || fs.isPending(e.ID) {
			return nil // a file here is that drive file (saved over it), or was changed since
		}
		// the drive announced it (feed or listing) before it could be
		// trashed: it goes here too
		fs.treeMu.Lock()
		err := fs.idx.Update(func(tx *index.Tx) error {
			_, err := tx.Delete(e.ID)
			if errors.Is(err, index.ErrNotFound) {
				return nil
			}
			return err
		})
		fs.treeMu.Unlock()
		if err != nil {
			return fmt.Errorf("vfs: forget what was made for a deleted file: %w", err)
		}
	}
	err := fs.api.TrashNode(ctx, sid)
	if err != nil && !driveapi.IsNotFound(err) {
		return fmt.Errorf("vfs: trash what was made for a deleted file: %w", err)
	}
	fs.touch(sid)
	fs.log.Info("vfs: deleted here while being sent: sent to the trash")
	return nil
}

// renameQueuedAfter reports a rename of op's node queued after op.
func (fs *FS) renameQueuedAfter(op *journal.Op) bool {
	ops, err := fs.jr.List()
	if err != nil {
		return true // the lookup it guards is only a little slower
	}
	for _, o := range ops {
		if o.Seq > op.Seq && o.NodeID == op.NodeID && o.Kind == journal.OpRename {
			return true
		}
	}
	return false
}

// gone reports a local id no longer in the index.
func (fs *FS) gone(id string) bool {
	_, err := fs.idx.Get(id)
	return errors.Is(err, index.ErrNotFound)
}

// earlierChangesIn reports unfinished changes queued before seq in a folder
// (to it, or renamed out of it). A name taken on the drive while they wait
// is most likely this PC's own (a file deleted or renamed here, not yet on
// the drive): wait for them rather than make a conflict copy.
func (fs *FS) earlierChangesIn(seq uint64, folder string) bool {
	ops, err := fs.jr.List()
	if err != nil {
		return false
	}
	for _, op := range ops {
		if op.Seq >= seq {
			break
		}
		if op.State == journal.StatePending && (op.ParentID == folder || op.FromParent == folder) {
			return true
		}
	}
	return false
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
	sid := ""
	if e, err := fs.idx.Get(op.NodeID); err == nil {
		sid = e.ServerID
	}
	fs.touch(op.ServerID, sid)
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
	// An op is written to the journal while its producer (create, save,
	// rename, delete) holds treeMu, and the index entry the op reads is
	// written just after. Wait for the producer, or a fast dispatcher reads
	// the entry as it was before (a saved temp file sent under its temp name).
	fs.treeMu.Lock()
	//lint:ignore SA2001 an empty critical section is the barrier
	fs.treeMu.Unlock()
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
	if op.ServerID != "" && fs.gone(op.NodeID) {
		// made on the drive, then deleted here before that was recorded
		if err := fs.trashMade(ctx, op.ServerID); err != nil {
			return err
		}
		return errObsolete
	}
	e, p, err := fs.entryAndParent(op)
	if err != nil {
		return err
	}
	if e.ServerID != "" {
		return nil
	}
	n, err := fs.api.CreateFolder(ctx, p.ServerID, e.Name, driveapi.ConflictFail, e.MTime)
	adopted := false
	if driveapi.IsNameConflict(err) && fs.earlierChangesIn(op.Seq, p.ID) {
		// most likely a folder or file of this name deleted or renamed
		// here, not yet on the drive: do not adopt what is about to go
		return errNotReady
	}
	if driveapi.IsNameConflict(err) {
		// replay after a crash, or made elsewhere meanwhile: adopt a folder of
		// that name; beside a file, make ours a conflict copy
		n, err = fs.adoptFolder(ctx, p.ServerID, e.Name)
		adopted = err == nil && n != nil && index.Fold(n.Name) == index.Fold(e.Name)
	}
	if driveapi.IsNotFound(err) {
		return &heldError{code: "folder_gone", reason: "The folder this was made in was deleted on the drive."}
	}
	if err != nil {
		return err
	}
	deleted := false
	fs.treeMu.Lock()
	err = fs.idx.Update(func(tx *index.Tx) error {
		cur, err := tx.Get(e.ID)
		if errors.Is(err, index.ErrNotFound) {
			deleted = true
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
		cur.ServerID = n.ID
		if cur.ParentID == e.ParentID && index.Fold(cur.Name) == index.Fold(e.Name) {
			// the drive's spelling (or the adopted folder's); a folder
			// renamed here while it was being made keeps its new name, which
			// the rename queued behind this op sends
			cur.Name = n.Name
		}
		cur.CanWrite, cur.CanUpload, cur.CanDelete, cur.Sealed = n.Can.Write, n.Can.Upload, n.Can.Delete, n.Sealed
		if err := tx.Put(cur); err != nil {
			return err
		}
		// a folder made here is empty on the drive; one adopted may hold
		// files the feed never told us about: list it when next opened
		return tx.SetListed(cur.ID, !adopted)
	})
	fs.treeMu.Unlock()
	if err != nil || !deleted || adopted {
		// (a folder that was already on the drive is not this PC's to trash)
		return err
	}
	// deleted here while it was being made: it goes to the trash (op
	// remembers it, in case that has to be tried again)
	op.ServerID = n.ID
	if err := fs.trashMade(ctx, n.ID); err != nil {
		return err
	}
	return nil
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
	if op.UploadID != "" && fs.gone(op.NodeID) {
		// deleted here after its upload started (and before that upload
		// was cleaned up, or the agent stopped meanwhile)
		if err := fs.discardUpload(ctx, op.UploadID); err != nil {
			return err
		}
		return errObsolete
	}
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
		UploadID: op.UploadID, Conflict: driveapi.ConflictFail,
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
		// a new version is addressed by name: when a rename of this file
		// is queued behind the upload, the drive still has it under its
		// old name (or in its old folder); send it there
		if fs.renameQueuedAfter(op) {
			nw, err := fs.api.GetNode(ctx, e.ServerID)
			if err != nil && !driveapi.IsNotFound(err) {
				return err
			}
			if err == nil && nw.Node.TrashedAt == nil && nw.Node.ParentID != "" {
				req.ParentID, req.Name = nw.Node.ParentID, nw.Node.Name
			}
		}
	}
	res, err := fs.eng.Upload(ctx, req)
	switch {
	case err == nil:
		return fs.finishUpload(ctx, e.ID, key, res.Node)
	case driveapi.IsNameConflict(err) && e.ServerID == "" && fs.earlierChangesIn(op.Seq, p.ID):
		// most likely a file of this name deleted or renamed here, not yet
		// on the drive: wait for that rather than make a conflict copy
		return errNotReady
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
	if err := fs.cache.CommitUpload(key, n.VersionID(), cache.Upload{Node: id, Check: checkOf(n)}); err != nil && !errors.Is(err, cache.ErrNotFound) {
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
			if n.CurrentVersion.ChecksumState == driveapi.ChecksumClaimed {
				// the drive checks large files later: remember that this
				// PC sent this version, so a mismatch can be re-uploaded
				if err := tx.SetMeta(claimKey(n.CurrentVersion.ID), cur.ID); err != nil {
					return err
				}
			}
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
		// deleted here while it was going up: delete wins. The op's
		// context is cancelled by then; run sends what was made to the
		// trash (see discardUpload).
		return errSentAfterDelete
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
	copyID := res.Node.ID // the copy's local id: a drive file's is its drive id
	if e.ServerID == "" {
		copyID = e.ID // our new file became the copy
	}
	if err := fs.cache.CommitUpload(key, res.Node.VersionID(), cache.Upload{Node: copyID, Check: checkOf(res.Node)}); err != nil && !errors.Is(err, cache.ErrNotFound) {
		fs.log.Warn("vfs: commit cache", "err", err)
	}
	deleted := false
	fs.treeMu.Lock()
	err = fs.idx.Update(func(tx *index.Tx) error {
		cur, err := tx.Get(e.ID)
		if errors.Is(err, index.ErrNotFound) {
			deleted = true
			return nil
		}
		if err != nil {
			return err
		}
		if cur.ServerID == "" {
			// our new file met a drive file of the same name: ours is the
			// copy (under the copy's name, unless it was renamed here
			// meanwhile: then the queued rename gives it that name)
			cur.ServerID, cur.VersionID = res.Node.ID, res.Node.VersionID()
			if cur.ParentID == e.ParentID && index.Fold(cur.Name) == index.Fold(e.Name) {
				cur.Name = res.Node.Name
			}
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
	if deleted {
		if e.ServerID == "" {
			// the copy was all of this new file: it goes too
			tctx, cancel := context.WithTimeout(fs.ctx, 30*time.Second)
			defer cancel()
			return fs.trashMade(tctx, res.Node.ID)
		}
		return nil
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
	if driveapi.IsNameConflict(err) && fs.earlierChangesIn(op.Seq, p.ID) {
		return errNotReady
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
		out = append(out, Held{Seq: op.Seq, Path: p, Kind: op.Kind, Code: op.HeldCode, Reason: op.Reason, Size: op.Size, Since: op.Queued,
			HasContent: fs.heldContent(op) != "", CanReupload: fs.canReupload(op)})
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
