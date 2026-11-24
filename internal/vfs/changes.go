package vfs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
)

const metaCursor = "changes_cursor"

// changesLoop reads the changes feed on SSE wake-ups and every poll
// interval, and probes the drive every few seconds while offline. A
// wake-up is read at the pace of fs.feed (see feedpace.go): one read at a
// time, a minimum interval apart, always one after the last wake-up.
func (fs *FS) changesLoop() {
	failures := 0
	for {
		err := fs.pollChanges(fs.ctx)
		fs.feed.done(time.Now(), fs.finished.Load())
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
			// wake-ups until the next read are covered by it
			if d := fs.feed.wait(time.Now()); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-fs.ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			select {
			case <-fs.wake:
			default:
			}
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
		if err := fs.feed.take(ctx); err != nil {
			return err
		}
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
		if err := fs.feed.take(ctx); err != nil {
			return err
		}
		before := fs.finished.Load()
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
		var outcomes []claimOutcome
		var others []driveapi.Change // not mere echoes of this PC's own changes
		fs.treeMu.Lock()
		err = fs.idx.Update(func(tx *index.Tx) error {
			relist, outcomes, others = relist[:0], outcomes[:0], others[:0]
			for i := range p.Changes {
				if ch := &p.Changes[i]; ch.Node != nil && fs.changedSince(ch.Node.ID, before) {
					// this PC changed it after the page was read: the page
					// may show it as it was (a later page has what it is)
					continue
				}
				if !fs.confirmsOwn(tx, &p.Changes[i]) {
					others = append(others, p.Changes[i])
				}
				if err := fs.applyChange(tx, &p.Changes[i], &relist); err != nil {
					return err
				}
				if o, err := checksumOutcome(tx, &p.Changes[i]); err != nil {
					return err
				} else if o != nil {
					outcomes = append(outcomes, *o)
				}
			}
			return tx.SetMeta(metaCursor, strconv.FormatInt(p.Next, 10))
		})
		fs.treeMu.Unlock()
		if err != nil {
			return err
		}
		fs.listingsMayBeStale(others)
		for _, id := range relist {
			fs.relist(id)
		}
		verified := false
		for _, o := range outcomes {
			check := cache.CheckVerified
			if o.damaged != nil {
				check = cache.CheckMismatch
			}
			if err := fs.cache.SetCheck(o.versionID, check); err != nil {
				fs.log.Warn("vfs: record the drive's checksum verdict", "err", err)
			}
			if o.damaged != nil {
				fs.holdMismatch(*o.damaged)
			} else {
				verified = true
			}
		}
		if verified {
			fs.releaseNow() // content the drive has just confirmed may go
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
				return fs.resyncFolder(tx, e.ID, relist)
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
			return nil // never seen here: listed when first opened
		}
		return fs.resyncFolder(tx, e.ID, relist)
	}
	return nil
}

// confirmsOwn reports a feed entry that only confirms a change this PC
// just made: an upsert, without resync, of a node changed here moments ago
// that the index already holds as the entry describes it. It changes
// nothing a listing in flight could undo, so it does not make one fetch
// again (during a bulk copy nearly every entry is one).
func (fs *FS) confirmsOwn(tx *index.Tx, ch *driveapi.Change) bool {
	n := ch.Node
	if ch.Op != driveapi.OpUpsert || ch.Resync != "" || n == nil || n.TrashedAt != nil || !fs.justChanged(n.ID) {
		return false
	}
	e, err := tx.BySID(n.ID)
	if err != nil || e.Name != n.Name || e.Kind != n.Kind || (!e.IsDir() && e.VersionID != n.ContentVersion()) {
		return false
	}
	parent, err := tx.Get(e.ParentID)
	return err == nil && parent.ServerID == n.ParentID
}

// listingsMayBeStale marks a listing in flight for a folder these changes
// touch to fetch again: its answer may predate them, and writing it would
// undo them (a child the feed added would be dropped as unseen).
func (fs *FS) listingsMayBeStale(changes []driveapi.Change) {
	fs.mu.Lock()
	n := len(fs.listing)
	fs.mu.Unlock()
	if n == 0 {
		return
	}
	var sids []string
	for i := range changes {
		ch := &changes[i]
		switch {
		case ch.Op == driveapi.OpResync:
			sids = append(sids, ch.FolderID)
		case ch.Node != nil:
			sids = append(sids, ch.Node.ParentID, ch.Node.ID)
		}
	}
	for _, sid := range sids {
		e, err := fs.idx.BySID(sid)
		if err != nil {
			continue
		}
		fs.mu.Lock()
		if c, ok := fs.listing[e.ID]; ok {
			c.again = true
		}
		fs.mu.Unlock()
	}
}

// resyncFolder forgets what is known under a folder the feed cannot
// describe. A folder this PC had listed is listed again now (even when the
// same page also upserts it, and even when a listing is in flight); one it
// had not is listed when first opened.
func (fs *FS) resyncFolder(tx *index.Tx, id string, relist *[]string) error {
	was := tx.Listed(id)
	if err := fs.forgetSubtree(tx, id); err != nil {
		return err
	}
	if was && !slices.Contains(*relist, id) {
		*relist = append(*relist, id)
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

// maxEagerRelists bounds how many visited folders a reset lists again at
// once (one after another, so the drive's rate limit is not hit); the rest
// are listed when next opened.
const maxEagerRelists = 32

// resetIndex runs when the feed cannot say what changed: every folder
// becomes unlisted, so each is listed again before it is next shown, and
// a listing reconciles what the index holds (kept until then, with any
// unsent local changes). The folders this PC had listed, the most recently
// listed first, are listed again in the background now.
func (fs *FS) resetIndex() {
	var visited []index.Listing
	fs.treeMu.Lock()
	err := fs.idx.Update(func(tx *index.Tx) error {
		var err error
		if visited, err = tx.Listings(); err != nil {
			return err
		}
		return tx.ForgetListings()
	})
	fs.treeMu.Unlock()
	if err != nil {
		fs.log.Warn("vfs: reset index", "err", err)
	}
	// a listing in flight may have been answered before the reset
	fs.mu.Lock()
	for _, c := range fs.listing {
		c.again = true
	}
	fs.mu.Unlock()
	ids := []string{fs.cfg.RootID}
	for _, l := range visited {
		if l.ID != fs.cfg.RootID && len(ids) < maxEagerRelists {
			ids = append(ids, l.ID)
		}
	}
	go fs.relistInOrder(ids)
}

// relistInOrder lists folders again one at a time, skipping any already
// listed meanwhile (opened by someone).
func (fs *FS) relistInOrder(ids []string) {
	ctx := fs.ctx
	if ctx == nil {
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if fs.idx.Listed(id) {
			continue
		}
		c := fs.relist(id)
		if c == nil {
			continue
		}
		select {
		case <-c.done:
		case <-ctx.Done():
			return
		}
	}
}

// ownEcho reports a change event about a node this PC changed moments ago:
// the drive telling this PC what it just did. It does not wake the changes
// loop (the next read, at the latest the regular poll, records it).
func (fs *FS) ownEcho(ev driveapi.StreamEvent) bool {
	var ch driveapi.StreamChange
	if json.Unmarshal(ev.Data, &ch) != nil || ch.NodeID == "" {
		return false
	}
	return fs.justChanged(ch.NodeID)
}

// streamLoop keeps the SSE stream open; every event (and every reconnect)
// wakes the changes loop.
func (fs *FS) streamLoop() {
	failures := 0
	for {
		err := fs.api.Stream(fs.ctx, func() { failures = 0; fs.wakeNow() }, func(ev driveapi.StreamEvent) error {
			if ev.Event == "change" && !fs.ownEcho(ev) {
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
