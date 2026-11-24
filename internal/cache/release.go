package cache

import (
	"time"
)

// Release after upload. Content written on this PC stays in the cache
// while it is unsent (dirty) and becomes the clean entry of its drive
// version when the upload completes (CommitUpload). The drive is the home
// of that content, so the cache gives it up soon after, instead of keeping
// it until the size cap forces an eviction:
//
//   - only once the drive has confirmed the bytes (checksum "verified");
//     content the drive took without checking ("claimed", or no checksum
//     state at all) is kept and, under cap pressure, evicted last;
//   - not while the content is open, read by an application recently, or
//     kept on this PC by a rule of the caller (keep-offline folders);
//   - only after a short grace period, so a file saved and opened again at
//     once does not download straight back.
//
// The rules are decided from metadata that is written with the commit
// (Sent, Check, Node), so they hold across a restart.

// ReleasePolicy are the timings of the release rules.
type ReleasePolicy struct {
	// AfterUpload is the grace period between an upload and the release of
	// its content (default 5 minutes).
	AfterUpload time.Duration
	// KeepOpened keeps content an application read within this long
	// (default 30 minutes).
	KeepOpened time.Duration
	// VerifyWait is how long a claimed upload is expected to be verified
	// (default 24 hours). After that it counts as ordinary cached content:
	// it is never released early, only evicted under cap pressure.
	VerifyWait time.Duration
}

// Default release timings.
const (
	DefaultAfterUpload = 5 * time.Minute
	DefaultKeepOpened  = 30 * time.Minute
	DefaultVerifyWait  = 24 * time.Hour
)

// WithDefaults fills in zero timings.
func (p ReleasePolicy) WithDefaults() ReleasePolicy {
	if p.AfterUpload <= 0 {
		p.AfterUpload = DefaultAfterUpload
	}
	if p.KeepOpened <= 0 {
		p.KeepOpened = DefaultKeepOpened
	}
	if p.VerifyWait <= 0 {
		p.VerifyWait = DefaultVerifyWait
	}
	return p
}

// KeepFunc says whether content must stay on this PC for a reason only
// the caller knows (the file is open, or it is in a keep-offline folder).
// It is called without the cache's lock held.
type KeepFunc func(key, node string) bool

// unconfirmed reports content this PC sent that the drive has not
// confirmed: it is never released early and is evicted last.
func unconfirmed(e *entry) bool {
	return !e.Sent.IsZero() && e.Check != CheckVerified
}

// releasableLocked reports clean content the release rules allow to go
// now (before the caller's keep rule).
func releasableLocked(e *entry, now time.Time, p ReleasePolicy) bool {
	return !e.Dirty && e.opens == 0 && !e.doomed && !e.Sent.IsZero() && e.Check == CheckVerified &&
		now.Sub(e.Sent) >= p.AfterUpload && now.Sub(e.Read) >= p.KeepOpened
}

// NoteRead records that an application read the content now. It is kept
// in memory and written with the next flush; losing it in a crash only
// makes a release come sooner.
func (h *Handle) NoteRead() {
	c := h.c
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(h.e.Read) < 10*time.Second {
		return // one metadata write per file every few seconds at most
	}
	h.e.Read = now
	h.e.metaDirty = true
}

// SetCheck records the drive's checksum verdict for a version this PC sent
// (from the changes feed). It is durable when it returns. Unknown keys and
// content not sent from this PC are left alone.
func (c *Cache) SetCheck(versionKey, check string) error {
	c.mu.Lock()
	e := c.entries[versionKey]
	if e == nil || e.doomed || e.Dirty || e.Sent.IsZero() || e.Check == check {
		c.mu.Unlock()
		return nil
	}
	e.Check = check
	snap := e.meta
	snap.Blocks = append([]byte(nil), e.Blocks...)
	seq, err := c.putLocked(versionKey, snap)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.waitMeta(seq)
}

type candidate struct {
	e         *entry
	key, node string
}

// ReleaseUploaded releases the content of uploads the drive has confirmed
// whose grace period is over, unless an application read them recently,
// they are open, or keep says so. It returns how many entries and bytes
// it released.
func (c *Cache) ReleaseUploaded(now time.Time, p ReleasePolicy, keep KeepFunc) (files int, bytes int64) {
	p = p.WithDefaults()
	return c.release(keep, func(e *entry) bool { return releasableLocked(e, now, p) })
}

// FreeNow releases everything the cache can give up at once: every clean
// entry that is not open, not kept (keep) and not an upload the drive has
// yet to confirm. The grace period and the recently-read rule do not apply:
// the person asked for the space. Unsent content is never touched.
func (c *Cache) FreeNow(keep KeepFunc) (files int, bytes int64) {
	return c.release(keep, func(e *entry) bool {
		return !e.Dirty && e.opens == 0 && !e.doomed && !unconfirmed(e)
	})
}

func (c *Cache) release(keep KeepFunc, ok func(*entry) bool) (files int, bytes int64) {
	c.mu.Lock()
	var cands []candidate
	for k, e := range c.entries {
		if ok(e) {
			cands = append(cands, candidate{e: e, key: k, node: e.Node})
		}
	}
	c.mu.Unlock()
	for _, cd := range cands {
		if keep != nil && keep(cd.key, cd.node) {
			continue
		}
		c.mu.Lock()
		// re-checked: it may have been opened or replaced meanwhile
		if c.entries[cd.key] == cd.e && ok(cd.e) {
			files++
			bytes += cd.e.bytes
			c.removeLocked(cd.e)
		}
		c.mu.Unlock()
	}
	if files > 0 {
		c.log.Info("cache: released content the drive has", "files", files, "bytes", bytes)
		// the removals are durable with the next metadata commit; a crash
		// before it drops the entries at the next Open (no data file)
		if err := c.Flush(); err != nil {
			c.log.Warn("cache: record released content", "err", err)
		}
	}
	return files, bytes
}

// Usage splits the cache's use the way the person sees it.
type Usage struct {
	Used int64
	Cap  int64
	// Unsent is content not yet on the drive.
	Unsent int64
	// Uploaded is content sent from this PC that is about to be released:
	// confirmed by the drive (in its grace period, or read recently), or
	// waiting for the drive's check (claimed, within VerifyWait).
	Uploaded int64
	// Opened is everything else: drive content opened on this PC, and
	// uploads the drive never confirmed. It is evicted, oldest first, only
	// when the cache is over its cap.
	Opened int64
}

// Usage reports the cache's use under policy p at time now.
func (c *Cache) Usage(now time.Time, p ReleasePolicy) Usage {
	p = p.WithDefaults()
	c.mu.Lock()
	defer c.mu.Unlock()
	u := Usage{Used: c.used, Cap: c.opts.Cap}
	for _, e := range c.entries {
		switch {
		case e.Dirty:
			u.Unsent += e.bytes
		case e.Sent.IsZero():
			u.Opened += e.bytes
		case e.Check == CheckVerified,
			e.Check == CheckClaimed && now.Sub(e.Sent) < p.VerifyWait:
			u.Uploaded += e.bytes
		default:
			u.Opened += e.bytes
		}
	}
	return u
}
