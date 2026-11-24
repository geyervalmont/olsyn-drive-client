package vfs

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
)

const metaCursor = "changes_cursor"

// changesLoop reads the changes feed on SSE wake-ups and every poll
// interval, and probes the drive every few seconds while offline.
func (fs *FS) changesLoop() {
	failures := 0
	for {
		err := fs.pollChanges(fs.ctx)
		wait := fs.cfg.PollInterval
		if err != nil && fs.ctx.Err() == nil {
			fs.noteErr(err)
			failures++
			wait = driveapi.Backoff{Base: time.Second, Max: 15 * time.Second}.Delay(failures, err)
			fs.log.Debug("vfs: changes feed", "code", errCode(err))
		} else if err == nil {
			failures = 0
			fs.noteOK()
		}
		select {
		case <-fs.ctx.Done():
			return
		case <-fs.wake:
		case <-time.After(wait):
		}
	}
}

// Refresh reads the changes feed now (also what an SSE wake-up does).
func (fs *FS) Refresh() { fs.wakeNow() }

func (fs *FS) pollChanges(ctx context.Context) error {
	cur := fs.idx.Meta(metaCursor)
	if cur == "" {
		// take the cursor BEFORE listing anything
		p, err := fs.api.Changes(ctx, -1, 0, "")
		if err != nil {
			return err
		}
		return fs.idx.SetMeta(metaCursor, strconv.FormatInt(p.Next, 10))
	}
	since, err := strconv.ParseInt(cur, 10, 64)
	if err != nil {
		since = -1
	}
	for {
		p, err := fs.api.Changes(ctx, since, driveapi.ChangesMaxPage, "")
		if err != nil {
			return err
		}
		if p.Reset != nil {
			fs.log.Info("vfs: changes feed reset", "reason", p.Reset.Reason)
			fs.resetIndex()
			return fs.idx.SetMeta(metaCursor, strconv.FormatInt(p.Next, 10))
		}
		var relist []string
		var damaged []mismatch
		fs.treeMu.Lock()
		err = fs.idx.Update(func(tx *index.Tx) error {
			relist, damaged = relist[:0], damaged[:0]
			for i := range p.Changes {
				if err := fs.applyChange(tx, &p.Changes[i], &relist); err != nil {
					return err
				}
				if m, err := checksumOutcome(tx, &p.Changes[i]); err != nil {
					return err
				} else if m != nil {
					damaged = append(damaged, *m)
				}
			}
			return tx.SetMeta(metaCursor, strconv.FormatInt(p.Next, 10))
		})
		fs.treeMu.Unlock()
		if err != nil {
			return err
		}
		for _, id := range relist {
			fs.relist(id)
		}
		for _, m := range damaged {
			fs.holdMismatch(m)
		}
		if !p.HasMore {
			return nil
		}
		since = p.Next
	}
}

func (fs *FS) applyChange(tx *index.Tx, ch *driveapi.Change, relist *[]string) error {
	switch ch.Op {
	case driveapi.OpUpsert:
		n := ch.Node
		if n == nil || n.ID == fs.cfg.RootID {
			return nil
		}
		if n.TrashedAt != nil {
			return fs.dropBySID(tx, n.ID)
		}
		parent, err := tx.BySID(n.ParentID)
		if errors.Is(err, index.ErrNotFound) || n.ParentID == "" {
			return fs.dropBySID(tx, n.ID) // moved out of our view
		}
		if err != nil {
			return err
		}
		if err := fs.mergeNode(tx, n, parent.ID); err != nil {
			return err
		}
		if ch.Resync == "subtree" && n.IsDir() {
			if e, err := tx.BySID(n.ID); err == nil {
				if err := fs.forgetSubtree(tx, e.ID); err != nil {
					return err
				}
				if tx.Listed(parent.ID) {
					*relist = append(*relist, e.ID)
				}
			}
		}
	case driveapi.OpTrashed:
		if ch.Node != nil {
			return fs.dropBySID(tx, ch.Node.ID)
		}
	case driveapi.OpRemoved:
		return fs.dropBySID(tx, ch.ID)
	case driveapi.OpResync:
		e, err := tx.BySID(ch.FolderID)
		if err != nil {
			return nil
		}
		if tx.Listed(e.ID) {
			if err := tx.SetListed(e.ID, false); err != nil {
				return err
			}
			*relist = append(*relist, e.ID)
		}
	}
	return nil
}

// dropBySID removes a node the drive no longer shows, unless unsent local
// changes live in it (then the queued change finds out and is held).
func (fs *FS) dropBySID(tx *index.Tx, sid string) error {
	e, err := tx.BySID(sid)
	if err != nil {
		return nil
	}
	if e.ID == fs.cfg.RootID || fs.isPendingTree(tx, e.ID) {
		return nil
	}
	_, err = tx.Delete(e.ID)
	return err
}

func (fs *FS) forgetSubtree(tx *index.Tx, id string) error {
	if err := tx.SetListed(id, false); err != nil {
		return err
	}
	kids, err := tx.Children(id)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.IsDir() {
			if err := fs.forgetSubtree(tx, k.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// resetIndex drops everything the drive told us (keeping unsent local
// changes and junk) and re-lists from the root.
func (fs *FS) resetIndex() {
	fs.treeMu.Lock()
	err := fs.idx.Update(func(tx *index.Tx) error {
		var ids []string
		if err := tx.All(func(e *index.Entry) error {
			if e.ID != fs.cfg.RootID && !e.LocalOnly && e.ServerID != "" {
				ids = append(ids, e.ID)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Get(id); err != nil {
				continue // removed with its folder
			}
			if fs.isPendingTree(tx, id) {
				continue
			}
			if _, err := tx.Delete(id); err != nil && !errors.Is(err, index.ErrNotFound) {
				return err
			}
		}
		return tx.ForgetListings()
	})
	fs.treeMu.Unlock()
	if err != nil {
		fs.log.Warn("vfs: reset index", "err", err)
	}
	fs.relist(fs.cfg.RootID)
}

// streamLoop keeps the SSE stream open; every event (and every reconnect)
// wakes the changes loop.
func (fs *FS) streamLoop() {
	failures := 0
	for {
		err := fs.api.Stream(fs.ctx, func() { failures = 0; fs.wakeNow() }, func(ev driveapi.StreamEvent) error {
			if ev.Event == "change" {
				fs.wakeNow()
			}
			return nil
		})
		if fs.ctx.Err() != nil {
			return
		}
		if driveapi.IsAuthLost(err) {
			fs.noteErr(err)
		}
		failures++
		select {
		case <-fs.ctx.Done():
			return
		case <-time.After(driveapi.Backoff{Base: time.Second, Max: 30 * time.Second}.Delay(failures, err)):
		}
	}
}
