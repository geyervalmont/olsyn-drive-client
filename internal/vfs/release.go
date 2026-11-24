package vfs

import (
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// The drive is the home of every file; this PC keeps what is still on its
// way up and what was opened recently. Content uploaded from here is
// released from the cache shortly after the drive has confirmed it (see
// cache.ReleaseUploaded for the rules); this file applies those rules with
// what only the file system knows: which files are open, and which are in
// keep-offline folders.

// checkOf maps the drive's checksum state of a version this PC sent onto
// the cache's.
func checkOf(n *driveapi.Node) string {
	if n == nil || n.CurrentVersion == nil {
		return ""
	}
	switch n.CurrentVersion.ChecksumState {
	case driveapi.ChecksumVerified:
		return cache.CheckVerified
	case driveapi.ChecksumClaimed:
		return cache.CheckClaimed
	}
	return "" // a drive without checksum states: never released early
}

// releaseLoop runs the release rules every ReleaseInterval.
func (fs *FS) releaseLoop() {
	t := time.NewTicker(fs.cfg.ReleaseInterval)
	defer t.Stop()
	for {
		select {
		case <-fs.ctx.Done():
			return
		case <-t.C:
			fs.releaseNow()
		}
	}
}

// releaseNow releases what the rules allow now.
func (fs *FS) releaseNow() {
	n, bytes := fs.cache.ReleaseUploaded(time.Now(), fs.ReleasePolicy(), fs.keepContent)
	if n > 0 {
		fs.log.Info("vfs: uploaded files cleared from this PC", "files", n, "bytes", bytes)
		fs.emit(Event{Kind: EventCache, Reason: "released"})
	}
}

// keepContent says whether a cache entry must stay for the file system's
// own reasons: its file is open, or it is in a keep-offline folder.
func (fs *FS) keepContent(_, node string) bool {
	if node == "" {
		return false
	}
	fs.mu.Lock()
	_, open := fs.open[node]
	fs.mu.Unlock()
	if open {
		return true
	}
	if keep := fs.cfg.KeepOffline; keep != nil {
		if p := fs.pathOf(node); p != "" && keep(p) {
			return true
		}
	}
	return false
}

// ReleasePolicy returns the timings in use.
func (fs *FS) ReleasePolicy() cache.ReleasePolicy {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.release
}

// SetReleasePolicy changes the timings (zero fields: the defaults) and
// applies them at once.
func (fs *FS) SetReleasePolicy(p cache.ReleasePolicy) {
	fs.mu.Lock()
	fs.release = p.WithDefaults()
	fs.mu.Unlock()
	if fs.ctx != nil {
		fs.releaseNow()
	}
}

// CacheUsage splits the cache's use: unsent, uploaded and about to be
// released, and opened.
func (fs *FS) CacheUsage() cache.Usage {
	return fs.cache.Usage(time.Now(), fs.ReleasePolicy())
}

// FreeCache releases everything that can go now (POST /v1/cache/free):
// every cached file that is not open, not kept offline, not unsent and not
// an upload the drive has yet to confirm.
func (fs *FS) FreeCache() (files int, bytes int64) {
	files, bytes = fs.cache.FreeNow(fs.keepContent)
	if files > 0 {
		fs.log.Info("vfs: cache freed on request", "files", files, "bytes", bytes)
		fs.emit(Event{Kind: EventCache, Reason: "freed"})
	}
	return files, bytes
}
