package vfs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
)

// Applications save in one of two ways that a drive must not see as a
// delete and a new file (the file would lose its history and its links):
//
//   - rename over: write a temp file, rename it over the original
//     (MoveFileEx with replace);
//   - Word and Excel: write a temp file, rename the original to another temp
//     name, rename the temp file to the original name, delete the original.
//
// Both end here as a new version of the drive file. The original's rename
// to a temp name is held back for displaceHold, so the temp file taking its
// name within that time finds it still unsent.

// displaceHold is how long a drive file's rename to a temp name waits
// before it is sent.
const displaceHold = 10 * time.Second

type displacedFile struct {
	nodeID string
	seq    uint64
	at     time.Time
}

// isSaveTemp reports names applications give an original while saving:
// junk temp names (~WRL0001.tmp) and Excel's eight hex digits.
func isSaveTemp(name string) bool {
	if IsJunk(name) {
		return true
	}
	if len(name) != 8 {
		return false
	}
	return strings.Trim(strings.ToUpper(name), "0123456789ABCDEF") == ""
}

func displacedKey(parentID, name string) string { return parentID + "\x00" + index.Fold(name) }

// noteDisplaced remembers that the drive file nodeID, called name in
// parentID, was renamed to a temp name by journal op seq.
func (fs *FS) noteDisplaced(parentID, name, nodeID string, seq uint64) {
	now := time.Now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for k, d := range fs.displaced {
		if now.Sub(d.at) > displaceHold {
			delete(fs.displaced, k)
		}
	}
	fs.displaced[displacedKey(parentID, name)] = displacedFile{nodeID: nodeID, seq: seq, at: now}
}

// takeDisplaced returns the drive file that was called name in parentID
// until it was renamed to a temp name moments ago, if its rename has not
// been sent (it is then left to adoptVersion to cancel).
func (fs *FS) takeDisplaced(parentID, name string) (*index.Entry, uint64) {
	key := displacedKey(parentID, name)
	fs.mu.Lock()
	d, ok := fs.displaced[key]
	delete(fs.displaced, key)
	_, running := fs.inflight[d.seq]
	fs.mu.Unlock()
	// a margin before the held-back rename may start
	if !ok || running || time.Since(d.at) > displaceHold-2*time.Second {
		return nil, 0
	}
	e, err := fs.idx.Get(d.nodeID)
	if err != nil || e.ServerID == "" || e.ParentID != parentID || !canModify(e) {
		return nil, 0
	}
	return e, d.seq
}

var errAdoptGone = errors.New("vfs: the file saved over is gone")

// adoptVersion makes src, a file not on the drive, a new version of the
// drive file orig, under name in dst. Replacing (displacedSeq 0), orig's
// entry goes; after Word's rename (displacedSeq is that rename), orig
// stays as a file on this PC only, under its temp name.
func (fs *FS) adoptVersion(ctx context.Context, src, orig *index.Entry, displacedSeq uint64, dst *index.Entry, name string) error {
	if displacedSeq != 0 {
		if err := fs.jr.Done(displacedSeq); err == nil {
			fs.doneLocked(journal.Op{NodeID: orig.ID})
		}
	} else {
		fs.dropOps(map[string]bool{orig.ID: true}, nil)
	}
	// the temp file's own changes (Excel's temp may already be queued as a
	// new file) are replaced by the upload below
	fs.dropOps(map[string]bool{src.ID: true}, nil)

	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()
	var cur, prev *index.Entry
	var removed []*index.Entry
	err := fs.idx.Update(func(tx *index.Tx) error {
		var err error
		if prev, err = tx.Get(orig.ID); err != nil {
			return errAdoptGone
		}
		if cur, err = tx.Get(src.ID); err != nil {
			return errAdoptGone
		}
		sid, ver := prev.ServerID, prev.VersionID
		if displacedSeq == 0 {
			if removed, err = tx.Delete(prev.ID); err != nil {
				return err
			}
		} else {
			prev.ServerID, prev.LocalOnly = "", true
			if err := tx.Put(prev); err != nil {
				return err
			}
		}
		cur.ParentID, cur.Name, cur.LocalOnly = dst.ID, name, false
		cur.ServerID, cur.VersionID = sid, ver
		cur.CanWrite, cur.CanUpload, cur.CanDelete, cur.Sealed, cur.Managed = prev.CanWrite, prev.CanUpload, prev.CanDelete, prev.Sealed, prev.Managed
		return tx.Put(cur)
	})
	if err != nil {
		return err
	}
	for _, r := range removed {
		if r.ContentKey != "" {
			_ = fs.cache.Remove(r.ContentKey)
		}
	}
	if cur.ContentKey != "" {
		if err := fs.queue(journal.Op{Kind: journal.OpUpload, NodeID: cur.ID, ParentID: cur.ParentID, Name: cur.Name,
			ContentKey: cur.ContentKey, Size: cur.Size, MTime: cur.MTime}); err != nil {
			return err
		}
	}
	fs.mu.Lock()
	on, onPrev := fs.open[cur.ID], fs.open[prev.ID]
	fs.mu.Unlock()
	if on != nil {
		on.mu.Lock()
		on.e.ParentID, on.e.Name, on.e.LocalOnly = cur.ParentID, cur.Name, false
		on.e.ServerID, on.e.VersionID = cur.ServerID, cur.VersionID
		on.e.CanWrite, on.e.CanUpload, on.e.CanDelete = cur.CanWrite, cur.CanUpload, cur.CanDelete
		on.mu.Unlock()
	}
	if onPrev != nil && displacedSeq != 0 {
		onPrev.mu.Lock()
		onPrev.e.ServerID, onPrev.e.LocalOnly = "", true
		onPrev.mu.Unlock()
	}
	fs.log.Info("vfs: saved over a drive file: sending a new version", "node", cur.ID)
	return nil
}
