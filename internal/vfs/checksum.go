package vfs

import (
	"context"
	"errors"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
)

// Large files are published by the drive as "claimed" and checked by its own
// checksum pass later. This PC remembers which versions it sent (index meta
// "claim:<version id>" = local id) until the pass reports "verified" or
// "mismatch" through the changes feed. A mismatch becomes a held change the
// person can re-upload from this PC's copy.

const claimPrefix = "claim:"

func claimKey(versionID string) string { return claimPrefix + versionID }

// Reasons shown for a version the drive's checksum pass found damaged.
const (
	reasonDamagedReupload = "The drive's copy of this file does not match what this PC sent. Re-upload it from this PC to replace the damaged copy."
	reasonDamagedLost     = "The drive's copy of this file does not match what this PC sent, and this PC no longer has the file. Copy it to the drive again from where it came from."
)

// mismatch is a version this PC sent that the drive found damaged.
type mismatch struct {
	localID, versionID string
	size               int64
}

// checksumOutcome settles a claim when the changes feed reports the result
// of the drive's checksum pass for a version this PC sent.
func checksumOutcome(tx *index.Tx, ch *driveapi.Change) (*mismatch, error) {
	if ch.Op != driveapi.OpUpsert || ch.Node == nil || ch.Node.CurrentVersion == nil || ch.Node.Kind != driveapi.KindFile {
		return nil, nil
	}
	v := ch.Node.CurrentVersion
	key := claimKey(v.ID)
	local := tx.Meta(key)
	if local == "" {
		return nil, nil
	}
	switch v.ChecksumState {
	case driveapi.ChecksumVerified:
		return nil, tx.DeleteMeta(key)
	case driveapi.ChecksumMismatch:
		if err := tx.DeleteMeta(key); err != nil {
			return nil, err
		}
		return &mismatch{localID: local, versionID: v.ID, size: v.Size}, nil
	}
	return nil, nil
}

// holdMismatch turns a damaged version into a held upload. When the cache
// still has every byte of the version, they are copied into unsent local
// content (protected from eviction) and the held change offers re-upload;
// otherwise it explains that the file must be copied again.
func (fs *FS) holdMismatch(m mismatch) {
	e, err := fs.idx.Get(m.localID)
	if err != nil || e.IsDir() || e.VersionID != m.versionID || e.ContentKey != "" || fs.isPending(e.ID) {
		// gone, or changed since: a newer version replaces the damaged one
		return
	}
	op := journal.Op{Kind: journal.OpUpload, NodeID: e.ID, ParentID: e.ParentID, Name: e.Name, MTime: e.MTime,
		Size: e.Size, BaseVersionID: m.versionID, State: journal.StateHeld, HeldCode: driveapi.CodeChecksumMismatch,
		Reason: reasonDamagedLost}
	if fs.cache.IsComplete(m.versionID) {
		key := newLocalID("local-")
		h, cerr := fs.cache.CreateLocal(key, m.versionID, e.Size)
		if cerr == nil {
			if len(h.Missing(0, h.Size())) > 0 {
				cerr = errors.New("cache copy incomplete")
			}
			_ = h.Close()
		}
		if cerr == nil {
			op.ContentKey, op.Reason = key, reasonDamagedReupload
		} else {
			_ = fs.cache.Remove(key)
			fs.log.Warn("vfs: keep a copy of a damaged upload", "err", cerr)
		}
	}
	if op.ContentKey != "" {
		fs.treeMu.Lock()
		err = fs.idx.Update(func(tx *index.Tx) error {
			cur, err := tx.Get(e.ID)
			if err != nil {
				return err
			}
			cur.ContentKey = op.ContentKey
			return tx.Put(cur)
		})
		fs.treeMu.Unlock()
		if err != nil {
			_ = fs.cache.Remove(op.ContentKey)
			fs.log.Warn("vfs: record a damaged upload", "err", err)
			return
		}
	}
	fs.mu.Lock()
	fs.pending[op.NodeID]++
	fs.mu.Unlock()
	op, err = fs.jr.Write(op)
	if err == nil {
		err = fs.jr.Sync(fs.ctx)
	}
	if err != nil {
		fs.doneLocked(op)
		fs.log.Warn("vfs: queue a damaged upload", "err", err)
		return
	}
	fs.log.Warn("vfs: drive reported a damaged upload", "seq", op.Seq, "reupload", op.ContentKey != "")
	fs.emit(Event{Kind: EventHeld, Seq: op.Seq, Path: fs.pathOf(op.NodeID), Reason: op.Reason})
}

// heldContent is the cache key holding a held upload's bytes, or "".
func (fs *FS) heldContent(op journal.Op) string {
	if op.Kind != journal.OpUpload {
		return ""
	}
	key := op.ContentKey
	if e, err := fs.idx.Get(op.NodeID); err == nil && e.ContentKey != "" {
		key = e.ContentKey
	}
	if key == "" || !fs.cache.Has(key) {
		return ""
	}
	return key
}

// canReupload: a held upload the drive received damaged, with its bytes
// still on this PC.
func (fs *FS) canReupload(op journal.Op) bool {
	return op.State == journal.StateHeld && op.HeldCode == driveapi.CodeChecksumMismatch && fs.heldContent(op) != ""
}

// Reupload queues a held checksum-mismatch upload again (from the bytes on
// this PC). The held item leaves the held list.
func (fs *FS) Reupload(ctx context.Context, seq uint64) error {
	op, err := fs.jr.Get(seq)
	if err != nil {
		return newErr(KindNotFound, "re-upload", "", err)
	}
	if op.State != journal.StateHeld {
		return newErr(KindInvalid, "re-upload", "", errors.New("only held changes can be uploaded again"))
	}
	if !fs.canReupload(op) {
		return newErr(KindInvalid, "re-upload", "", errors.New("this held file cannot be uploaded again from this PC"))
	}
	op.State, op.HeldCode, op.Reason = journal.StatePending, "", ""
	op.Attempts, op.LastError, op.UploadID, op.NotBefore = 0, "", "", time.Time{}
	if err := fs.jr.Update(op); err != nil {
		return newErr(KindOther, "re-upload", "", err)
	}
	if err := fs.jr.Sync(ctx); err != nil {
		return newErr(KindOther, "re-upload", "", err)
	}
	fs.log.Info("vfs: held change queued again", "seq", seq)
	fs.emit(Event{Kind: EventReleased, Seq: seq, Path: fs.pathOf(op.NodeID), Reason: "reupload"})
	fs.kickNow()
	return nil
}
