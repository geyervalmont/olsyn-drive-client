// Package cache keeps file content on this PC: sparse files keyed by drive
// version id (or by a local key for content not yet sent), a block bitmap of
// what is present, a size cap with least-recently-used eviction that never
// evicts open or unsent data, and back-pressure when unsent data fills the
// cap.
//
// Crash safety: data is written first and the bitmap persisted only after
// the data file is fsynced (Flush, Sync), so after a crash the bitmap may
// under-report what is present (it is fetched again) but never over-reports.
// Unsent (dirty) content is synced by the caller before it is journalled.
//
// Metadata writes never happen under the cache's lock: they are queued in
// memory (in the order the in-memory state changed) and written in one
// database transaction by the next caller that needs them durable (Sync,
// Flush, Commit, Adopt), so concurrent callers share a commit and opening
// or writing a file never waits behind an fsync of another.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Options configure a cache.
type Options struct {
	// Dir holds cache.db and the data files.
	Dir string
	// Cap is the size cap in bytes (clean data is evicted above it).
	Cap int64
	// BlockSize is the bitmap granularity. Default 1 MiB.
	BlockSize int64
	// BackpressureTimeout bounds how long WaitForRoom waits for unsent data
	// to drain before giving up with ErrNoSpace. Default 60 s.
	BackpressureTimeout time.Duration
	Logger              *slog.Logger
}

// Range is a byte range.
type Range struct {
	Off int64
	Len int64
}

// Errors.
var (
	ErrNotFound = errors.New("cache: no such entry")
	ErrNoSpace  = errors.New("cache: unsent data fills the cache")
	ErrClean    = errors.New("cache: entry holds drive content and is read-only")
	ErrMissing  = errors.New("cache: range not present")
)

var bEntries = []byte("entries")

type meta struct {
	File      string    `json:"file"`
	Size      int64     `json:"size"`
	BaseLimit int64     `json:"base"` // bytes [0,BaseLimit) come from the drive; beyond are local (zeros or written)
	Blocks    []byte    `json:"blocks"`
	LastUsed  time.Time `json:"used"`
	Dirty     bool      `json:"dirty"`
}

type entry struct {
	key string
	meta
	opens     int
	f         *os.File
	metaDirty bool
	written   bool // data written since the last fsync
	doomed    bool // remove when the last handle closes
	bytes     int64
	wgen      uint64 // bumped on every data or bitmap change
}

// Cache is the content cache.
type Cache struct {
	opts    Options
	dataDir string
	db      *bolt.DB
	log     *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	orphans map[string]bool // data files no entry owns (since Open)
	used    int64
	changed chan struct{}     // closed and replaced when space is freed
	metaQ   map[string][]byte // metadata not yet written (nil value: delete)
	metaSeq uint64            // bumped by every queued metadata change

	// metaWriteMu serialises metadata commits; metaDone is the last
	// metaSeq known written.
	metaWriteMu sync.Mutex
	metaDone    uint64
}

// Open opens (or creates) the cache in opts.Dir.
func Open(opts Options) (*Cache, error) {
	if opts.BlockSize <= 0 {
		opts.BlockSize = 1 << 20
	}
	if opts.BackpressureTimeout <= 0 {
		opts.BackpressureTimeout = 60 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	dataDir := filepath.Join(opts.Dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: create %s: %w", dataDir, err)
	}
	db, err := bolt.Open(filepath.Join(opts.Dir, "cache.db"), 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cache: open db: %w", err)
	}
	c := &Cache{opts: opts, dataDir: dataDir, db: db, log: opts.Logger, entries: map[string]*entry{}, changed: make(chan struct{}),
		metaQ: map[string][]byte{}}
	if err := c.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) load() error {
	known := map[string]bool{}
	var drop []string
	err := c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bEntries)
		if err != nil {
			return err
		}
		return b.ForEach(func(k, v []byte) error {
			var m meta
			if err := json.Unmarshal(v, &m); err != nil {
				drop = append(drop, string(k))
				return nil
			}
			if _, err := os.Stat(filepath.Join(c.dataDir, m.File)); err != nil {
				if m.Dirty {
					c.log.Error("cache: unsent content lost", "key", string(k))
				}
				drop = append(drop, string(k))
				return nil
			}
			e := &entry{key: string(k), meta: m}
			e.bytes = c.bytesOf(e)
			c.entries[e.key] = e
			c.used += e.bytes
			known[m.File] = true
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("cache: load: %w", err)
	}
	if len(drop) > 0 {
		_ = c.db.Update(func(tx *bolt.Tx) error {
			for _, k := range drop {
				_ = tx.Bucket(bEntries).Delete([]byte(k))
			}
			return nil
		})
	}
	// data files no entry owns: written before a crash, never recorded.
	// Unsent content among them is adopted (Adopt); DropOrphans removes the rest.
	c.orphans = map[string]bool{}
	files, _ := os.ReadDir(c.dataDir)
	for _, f := range files {
		if !known[f.Name()] {
			c.orphans[f.Name()] = true
		}
	}
	return nil
}

// Adopt registers unsent content whose data file survived a crash but whose
// metadata did not (the journal knows its key and size). It only works for
// content without a drive base (new files, overwritten files), which is all
// present by construction. It returns ErrNotFound when there is no such file.
func (c *Cache) Adopt(key string, size int64) error {
	c.mu.Lock()
	if e := c.entries[key]; e != nil {
		c.mu.Unlock()
		return nil
	}
	name := fileName(key, true)
	if !c.orphans[name] {
		c.mu.Unlock()
		return ErrNotFound
	}
	st, err := os.Stat(filepath.Join(c.dataDir, name))
	if err != nil || st.Size() < size {
		c.mu.Unlock()
		return ErrNotFound
	}
	delete(c.orphans, name)
	e := &entry{key: key, meta: meta{File: name, Size: size, Dirty: true, LastUsed: time.Now()}, metaDirty: true}
	c.entries[key] = e
	c.recount(e)
	seq, err := c.putLocked(key, e.meta)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.waitMeta(seq)
}

// DropOrphans deletes data files nothing claimed after a crash.
func (c *Cache) DropOrphans() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.orphans {
		_ = os.Remove(filepath.Join(c.dataDir, name))
	}
	c.orphans = map[string]bool{}
}

// Abandon closes the cache as a crash would: nothing is flushed. For tests
// that simulate a killed process.
func (c *Cache) Abandon() {
	c.mu.Lock()
	for _, e := range c.entries {
		if e.f != nil {
			_ = e.f.Close()
			e.f = nil
		}
	}
	c.metaQ = map[string][]byte{} // queued metadata dies with the process
	c.mu.Unlock()
	_ = c.db.Close()
}

// Close flushes and closes the cache.
func (c *Cache) Close() error {
	err := c.Flush()
	c.mu.Lock()
	for _, e := range c.entries {
		if e.f != nil {
			_ = e.f.Close()
			e.f = nil
		}
	}
	c.mu.Unlock()
	if cerr := c.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// fileName names a data file. Local (unsent) content gets a name derived
// from its key alone, so it can be adopted after a crash from the journal
// (Adopt); drive versions get a fresh name each time (an old file of the
// same version may still be open).
func fileName(key string, local bool) string {
	seed := key
	if !local {
		seed += "|" + time.Now().String()
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

func (c *Cache) nblocks(size int64) int64 {
	return (size + c.opts.BlockSize - 1) / c.opts.BlockSize
}

func (e *entry) has(b int64) bool {
	i := b / 8
	return i < int64(len(e.Blocks)) && e.Blocks[i]&(1<<(b%8)) != 0
}

func (e *entry) set(b int64, on bool) {
	i := b / 8
	for int64(len(e.Blocks)) <= i {
		e.Blocks = append(e.Blocks, 0)
	}
	if on {
		e.Blocks[i] |= 1 << (b % 8)
	} else {
		e.Blocks[i] &^= 1 << (b % 8)
	}
}

// valid reports a block whose bytes are all known locally.
func (c *Cache) valid(e *entry, b int64) bool {
	return e.has(b) || b*c.opts.BlockSize >= e.BaseLimit
}

func (c *Cache) bytesOf(e *entry) int64 {
	if e.Dirty {
		return e.Size
	}
	var n int64
	for _, x := range e.Blocks {
		n += int64(bits.OnesCount8(x))
	}
	n *= c.opts.BlockSize
	return min(n, e.Size)
}

func (c *Cache) recount(e *entry) {
	nb := c.bytesOf(e)
	c.used += nb - e.bytes
	freed := nb < e.bytes
	e.bytes = nb
	if freed {
		c.signalLocked()
	}
}

func (c *Cache) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Handle is an open cache entry.
type Handle struct {
	c      *Cache
	e      *entry
	closed bool
}

func (c *Cache) openFileLocked(e *entry) error {
	if e.f != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(c.dataDir, e.File), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("cache: open data file: %w", err)
	}
	if err := setSparse(f); err != nil {
		c.log.Debug("cache: sparse flag not set", "err", err)
	}
	e.f = f
	return nil
}

// Get opens the clean entry for a drive version, creating an empty one of
// the given size when missing.
func (c *Cache) Get(key string, size int64) (*Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || e.doomed {
		e = &entry{key: key, meta: meta{File: fileName(key, false), Size: size, BaseLimit: size}, metaDirty: true}
		c.entries[key] = e
	}
	if err := c.openFileLocked(e); err != nil {
		if e.opens == 0 && e.bytes == 0 {
			delete(c.entries, key)
		}
		return nil, err
	}
	e.opens++
	e.LastUsed = time.Now()
	return &Handle{c: c, e: e}, nil
}

// Open opens an existing entry (clean or dirty).
func (c *Cache) Open(key string) (*Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || e.doomed {
		return nil, ErrNotFound
	}
	if err := c.openFileLocked(e); err != nil {
		return nil, err
	}
	e.opens++
	e.LastUsed = time.Now()
	return &Handle{c: c, e: e}, nil
}

// CreateLocal makes a new dirty entry for content written on this PC. With a
// base key, it starts as a copy of that version (size baseSize): blocks the
// cache holds are copied, the rest are fetched on demand (MissingForWrite /
// Missing give the ranges, read from the base version). Without, it is empty.
func (c *Cache) CreateLocal(key, baseKey string, baseSize int64) (*Handle, error) {
	c.mu.Lock()
	if _, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("cache: entry %s exists", key)
	}
	e := &entry{key: key, meta: meta{File: fileName(key, true), Dirty: true, LastUsed: time.Now()}, metaDirty: true}
	var base *entry
	if baseKey != "" {
		e.Size, e.BaseLimit = baseSize, baseSize
		base = c.entries[baseKey]
		if base != nil && !base.doomed {
			if err := c.openFileLocked(base); err != nil {
				c.mu.Unlock()
				return nil, err
			}
			base.opens++
		} else {
			base = nil
		}
	}
	if err := c.openFileLocked(e); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	e.opens++
	c.entries[key] = e
	c.mu.Unlock()

	h := &Handle{c: c, e: e}
	if base != nil {
		if base.Dirty {
			// unsent content (e.g. being uploaded): copy all of it; the new
			// entry has no drive base
			c.mu.Lock()
			e.BaseLimit = 0
			c.mu.Unlock()
		}
		err := c.cloneBlocks(base, e)
		bh := &Handle{c: c, e: base}
		_ = bh.Close()
		if err != nil {
			_ = h.Close()
			_ = c.Remove(key)
			return nil, err
		}
	}
	if err := e.f.Truncate(e.Size); err != nil {
		_ = h.Close()
		_ = c.Remove(key)
		return nil, fmt.Errorf("cache: size new entry: %w", err)
	}
	c.mu.Lock()
	c.recount(e)
	c.mu.Unlock()
	return h, nil
}

func (c *Cache) cloneBlocks(from, to *entry) error {
	buf := make([]byte, c.opts.BlockSize)
	for b := int64(0); b < c.nblocks(to.Size); b++ {
		c.mu.Lock()
		ok := from.has(b) || (from.Dirty && c.valid(from, b))
		c.mu.Unlock()
		if !ok {
			continue
		}
		off := b * c.opts.BlockSize
		n := min(c.opts.BlockSize, to.Size-off)
		if _, err := from.f.ReadAt(buf[:n], off); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("cache: copy base block: %w", err)
		}
		if _, err := to.f.WriteAt(buf[:n], off); err != nil {
			return fmt.Errorf("cache: copy base block: %w", err)
		}
		c.mu.Lock()
		to.set(b, true)
		c.mu.Unlock()
	}
	to.written = true
	return nil
}

// Key is the entry's key.
func (h *Handle) Key() string { return h.e.key }

// Size is the content size.
func (h *Handle) Size() int64 {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.e.Size
}

// Dirty reports content not yet on the drive.
func (h *Handle) Dirty() bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.e.Dirty
}

// Complete reports every byte present locally.
func (h *Handle) Complete() bool {
	return len(h.Missing(0, h.Size())) == 0
}

// Missing returns the ranges in [off, off+n) (clipped to the size) that must
// be fetched from the drive before reading, block-aligned and merged.
func (h *Handle) Missing(off, n int64) []Range {
	c := h.c
	c.mu.Lock()
	defer c.mu.Unlock()
	end := min(off+n, h.e.Size)
	return c.missingLocked(h.e, off, end, nil)
}

func (c *Cache) missingLocked(e *entry, off, end int64, skip func(b, bs, be int64) bool) []Range {
	var out []Range
	if off >= end {
		return nil
	}
	bs := c.opts.BlockSize
	for b := off / bs; b*bs < end; b++ {
		if c.valid(e, b) {
			continue
		}
		start := b * bs
		stop := min(start+bs, e.BaseLimit)
		if skip != nil && skip(b, start, stop) {
			continue
		}
		if k := len(out); k > 0 && out[k-1].Off+out[k-1].Len == start {
			out[k-1].Len += stop - start
		} else {
			out = append(out, Range{Off: start, Len: stop - start})
		}
	}
	return out
}

// MissingForWrite returns the base ranges a write of [off, off+n) needs
// first: blocks the write only partly covers whose base bytes are not here.
func (h *Handle) MissingForWrite(off, n int64) []Range {
	c := h.c
	c.mu.Lock()
	defer c.mu.Unlock()
	end := min(off+n, h.e.BaseLimit)
	return c.missingLocked(h.e, off, end, func(b, start, stop int64) bool {
		return off <= start && off+n >= stop // fully overwritten
	})
}

// ReadAt reads present bytes. Reading a missing range fails with ErrMissing.
func (h *Handle) ReadAt(p []byte, off int64) (int, error) {
	c := h.c
	c.mu.Lock()
	size := h.e.Size
	end := min(off+int64(len(p)), size)
	miss := c.missingLocked(h.e, off, end, nil)
	f := h.e.f
	c.mu.Unlock()
	if off >= size {
		return 0, io.EOF
	}
	if len(miss) > 0 {
		return 0, ErrMissing
	}
	n, err := f.ReadAt(p[:end-off], off)
	if errors.Is(err, io.EOF) && int64(n) == end-off {
		err = nil
	}
	if err == nil && int64(n) < int64(len(p)) {
		err = io.EOF
	}
	return n, err
}

// Fill stores bytes fetched from the drive at off and marks the blocks they
// complete as present. Bytes past the base limit are not written (they are
// local data or zeros).
func (h *Handle) Fill(off int64, data []byte) error {
	c := h.c
	c.mu.Lock()
	limit := h.e.BaseLimit
	f := h.e.f
	c.mu.Unlock()
	if off+int64(len(data)) > limit {
		if off >= limit {
			return nil
		}
		data = data[:limit-off]
	}
	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("cache: fill: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bs := c.opts.BlockSize
	end := off + int64(len(data))
	for b := off / bs; b*bs < end; b++ {
		start, stop := b*bs, min((b+1)*bs, h.e.BaseLimit)
		if off <= start && end >= stop {
			h.e.set(b, true)
		}
	}
	h.e.written, h.e.metaDirty = true, true
	h.e.wgen++
	h.e.LastUsed = time.Now()
	c.recount(h.e)
	return nil
}

// WriteAt writes local data (dirty entries only). Call MissingForWrite and
// Fill first when the entry has a base.
func (h *Handle) WriteAt(p []byte, off int64) (int, error) {
	c := h.c
	c.mu.Lock()
	if !h.e.Dirty {
		c.mu.Unlock()
		return 0, ErrClean
	}
	f := h.e.f
	c.mu.Unlock()
	n, err := f.WriteAt(p, off)
	c.mu.Lock()
	defer c.mu.Unlock()
	end := off + int64(n)
	bs := c.opts.BlockSize
	for b := off / bs; b*bs < end; b++ {
		h.e.set(b, true) // partial blocks were filled first (or lie past the base)
	}
	if end > h.e.Size {
		h.e.Size = end
	}
	h.e.written, h.e.metaDirty = true, true
	h.e.wgen++
	h.e.LastUsed = time.Now()
	c.recount(h.e)
	if err != nil {
		return n, fmt.Errorf("cache: write: %w", err)
	}
	return n, nil
}

// Truncate sets the size of a dirty entry. Shrinking forgets the base
// beyond the new size; growing adds zeros.
func (h *Handle) Truncate(size int64) error {
	c := h.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if !h.e.Dirty {
		return ErrClean
	}
	if err := h.e.f.Truncate(size); err != nil {
		return fmt.Errorf("cache: truncate: %w", err)
	}
	bs := c.opts.BlockSize
	if size < h.e.BaseLimit {
		h.e.BaseLimit = size
	}
	if size < h.e.Size {
		// blocks wholly past the end are gone
		for b := (size + bs - 1) / bs; b < c.nblocks(h.e.Size); b++ {
			h.e.set(b, false)
		}
	}
	h.e.Size = size
	h.e.written, h.e.metaDirty = true, true
	h.e.wgen++
	c.recount(h.e)
	return nil
}

// Sync makes the entry's data and bitmap durable.
func (h *Handle) Sync() error {
	seq, err := h.c.syncEntry(h.e)
	if err != nil {
		return err
	}
	return h.c.waitMeta(seq)
}

// syncEntry fsyncs the data file, then queues the bitmap as it was before
// the fsync, so the stored bitmap never claims bytes that are not durable.
// The bitmap is durable once waitMeta(seq) returns.
func (c *Cache) syncEntry(e *entry) (uint64, error) {
	c.mu.Lock()
	snap := e.meta
	snap.Blocks = append([]byte(nil), e.Blocks...)
	gen, f, written, dirtyMeta := e.wgen, e.f, e.written, e.metaDirty
	c.mu.Unlock()
	if !written && !dirtyMeta {
		return 0, nil
	}
	if written {
		if f == nil {
			tf, err := os.OpenFile(filepath.Join(c.dataDir, snap.File), os.O_RDWR, 0)
			if err != nil {
				return 0, fmt.Errorf("cache: sync data: %w", err)
			}
			err = tf.Sync()
			_ = tf.Close()
			if err != nil {
				return 0, fmt.Errorf("cache: sync data: %w", err)
			}
		} else if err := f.Sync(); err != nil {
			return 0, fmt.Errorf("cache: sync data: %w", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.doomed || c.entries[e.key] != e {
		return 0, nil
	}
	seq, err := c.putLocked(e.key, snap)
	if err != nil {
		return 0, err
	}
	if e.wgen == gen {
		e.written, e.metaDirty = false, false
	}
	return seq, nil
}

// putLocked queues one entry's metadata. Changes are queued under c.mu, so
// their order matches the in-memory order; waitMeta writes them.
func (c *Cache) putLocked(key string, m meta) (uint64, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return 0, fmt.Errorf("cache: persist: %w", err)
	}
	c.metaQ[key] = b
	c.metaSeq++
	return c.metaSeq, nil
}

// forgetLocked queues the removal of an entry's metadata.
func (c *Cache) forgetLocked(key string) uint64 {
	c.metaQ[key] = nil
	c.metaSeq++
	return c.metaSeq
}

// waitMeta returns once every metadata change up to seq is written. The
// first caller writes everything queued in one transaction; callers behind
// it usually find their change already written.
func (c *Cache) waitMeta(seq uint64) error {
	c.metaWriteMu.Lock()
	defer c.metaWriteMu.Unlock()
	if c.metaDone >= seq {
		return nil
	}
	c.mu.Lock()
	q, upTo := c.metaQ, c.metaSeq
	c.metaQ = map[string][]byte{}
	c.mu.Unlock()
	if len(q) > 0 {
		err := c.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bEntries)
			for k, v := range q {
				var err error
				if v == nil {
					err = b.Delete([]byte(k))
				} else {
					err = b.Put([]byte(k), v)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			// put back what no newer change replaced, for the next attempt
			c.mu.Lock()
			for k, v := range q {
				if _, newer := c.metaQ[k]; !newer {
					c.metaQ[k] = v
				}
			}
			c.mu.Unlock()
			return fmt.Errorf("cache: persist: %w", err)
		}
	}
	c.metaDone = upTo
	return nil
}

// Close releases the handle. The last close of an entry syncs what was
// written to it and closes its file.
func (h *Handle) Close() error {
	c := h.c
	c.mu.Lock()
	if h.closed {
		c.mu.Unlock()
		return nil
	}
	h.closed = true
	e := h.e
	e.opens--
	e.LastUsed = time.Now()
	last := e.opens == 0
	c.mu.Unlock()
	if !last {
		return nil
	}
	// No sync here: unsent content was synced by its writer (Sync or
	// SyncData) before it was queued, and fetched drive content is made
	// durable by the next Flush (until then the bitmap on disk simply does
	// not claim it).
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.opens == 0 && e.f != nil {
		_ = e.f.Close()
		e.f = nil
	}
	if e.doomed && e.opens == 0 {
		c.removeLocked(e)
	}
	c.signalLocked() // an entry stopped being open: it may now drain or be evicted
	return nil
}

// SyncData makes the written data durable without recording the bitmap:
// enough for content Adopt can recover (no drive base). Use Sync otherwise.
func (h *Handle) SyncData() error {
	c, e := h.c, h.e
	c.mu.Lock()
	f, gen := e.f, e.wgen
	c.mu.Unlock()
	if f == nil {
		return nil
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("cache: sync data: %w", err)
	}
	c.mu.Lock()
	if e.wgen == gen {
		e.written = false // the bitmap still needs persisting (metaDirty)
	}
	c.mu.Unlock()
	return nil
}

// Adoptable reports content Adopt could recover from its data file alone.
func (h *Handle) Adoptable() bool {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.e.Dirty && h.e.BaseLimit == 0
}

// Commit turns a dirty entry into the clean entry of the drive version it
// became (after a successful upload). Returns ErrNotFound for an unknown key.
func (c *Cache) Commit(localKey, versionKey string) error {
	c.mu.Lock()
	e := c.entries[localKey]
	if e == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	if old := c.entries[versionKey]; old != nil && old != e {
		if old.opens > 0 {
			old.doomed = true
			delete(c.entries, versionKey)
		} else {
			c.removeLocked(old)
		}
	}
	snap := e.meta
	snap.Dirty = false
	snap.BaseLimit = snap.Size
	snap.Blocks = nil
	tmp := &entry{meta: snap}
	for b := int64(0); b < c.nblocks(snap.Size); b++ {
		tmp.set(b, true)
	}
	snap.Blocks = tmp.Blocks
	// both changes are queued together, so they are written in one
	// transaction
	c.forgetLocked(localKey)
	seq, err := c.putLocked(versionKey, snap)
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("cache: commit: %w", err)
	}
	delete(c.entries, localKey)
	e.key = versionKey
	e.meta = snap
	c.entries[versionKey] = e
	c.recount(e)
	c.signalLocked()
	c.evictLocked(0)
	c.mu.Unlock()
	if err := c.waitMeta(seq); err != nil {
		return fmt.Errorf("cache: commit: %w", err)
	}
	return nil
}

// Remove deletes an entry (at once, or when its last handle closes).
func (c *Cache) Remove(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil
	}
	if e.opens > 0 {
		e.doomed = true
		return nil
	}
	c.removeLocked(e)
	return nil
}

func (c *Cache) removeLocked(e *entry) {
	if cur := c.entries[e.key]; cur == e {
		delete(c.entries, e.key)
	}
	if e.f != nil {
		_ = e.f.Close()
		e.f = nil
	}
	_ = os.Remove(filepath.Join(c.dataDir, e.File))
	c.used -= e.bytes
	e.bytes = 0
	c.forgetLocked(e.key) // written with the next metadata commit
	c.signalLocked()
}

// Has reports an entry for key.
func (c *Cache) Has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	return e != nil && !e.doomed
}

// IsComplete reports that every byte of key is present (no fetch needed).
func (c *Cache) IsComplete(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil || e.doomed {
		return false
	}
	return len(c.missingLocked(e, 0, e.Size, nil)) == 0
}

// Stats describe the cache's use.
type Stats struct {
	Used    int64 // bytes held
	Unsent  int64 // bytes of dirty entries
	Cap     int64
	Entries int
}

// Stats returns current use.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Stats{Used: c.used, Cap: c.opts.Cap, Entries: len(c.entries)}
	for _, e := range c.entries {
		if e.Dirty {
			s.Unsent += e.bytes
		}
	}
	return s
}

// SetCap changes the size cap and evicts down to it.
func (c *Cache) SetCap(n int64) {
	c.mu.Lock()
	c.opts.Cap = n
	c.mu.Unlock()
	c.Evict()
}

// Evict removes least-recently-used clean, closed entries until the cache
// is under its cap. Open entries and unsent data are never evicted.
func (c *Cache) Evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictLocked(0)
}

func (c *Cache) evictLocked(want int64) {
	if c.opts.Cap <= 0 || c.used+want <= c.opts.Cap {
		return
	}
	var cands []*entry
	for _, e := range c.entries {
		if !e.Dirty && e.opens == 0 && !e.doomed {
			cands = append(cands, e)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].LastUsed.Before(cands[j].LastUsed) })
	for _, e := range cands {
		if c.used+want <= c.opts.Cap {
			return
		}
		c.log.Debug("cache: evict", "bytes", e.bytes)
		c.removeLocked(e)
	}
}

// WaitForRoom is the back-pressure for writes: before adding n bytes of
// unsent data it evicts clean data and, if unsent data that is already
// queued for upload still fills the cap, waits for uploads to drain it.
// Data of files still open never blocks (one file larger than the cap
// must still be writable). After BackpressureTimeout it returns ErrNoSpace.
func (c *Cache) WaitForRoom(ctx context.Context, n int64) error {
	timer := time.NewTimer(c.opts.BackpressureTimeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		c.evictLocked(n)
		if c.opts.Cap <= 0 || c.used+n <= c.opts.Cap {
			c.mu.Unlock()
			return nil
		}
		var draining int64
		for _, e := range c.entries {
			if e.Dirty && e.opens == 0 {
				draining += e.bytes
			}
		}
		ch := c.changed
		c.mu.Unlock()
		if draining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrNoSpace
		case <-ch:
		}
	}
}

// Flush fsyncs written data files and persists every changed bitmap (and
// every other queued metadata change) in one transaction.
func (c *Cache) Flush() error { return c.FlushOlder(0) }

// FlushOlder is Flush for the periodic flush loop: it leaves alone new
// content (unsent, no drive base) written within the last age. Such content
// is usually sent within seconds, its journal record carries a checksum
// that recovery checks, and an fsync per new file would compete with the
// app saving the next ones. Content still unsent after age is synced.
func (c *Cache) FlushOlder(age time.Duration) error {
	now := time.Now()
	c.mu.Lock()
	var es []*entry
	for _, e := range c.entries {
		if !e.metaDirty && !e.written {
			continue
		}
		if age > 0 && e.Dirty && e.BaseLimit == 0 && now.Sub(e.LastUsed) < age {
			continue
		}
		es = append(es, e)
	}
	c.mu.Unlock()
	var first error
	for _, e := range es {
		if _, err := c.syncEntry(e); err != nil && first == nil {
			first = err
		}
	}
	c.mu.Lock()
	seq := c.metaSeq
	c.mu.Unlock()
	if err := c.waitMeta(seq); err != nil && first == nil {
		first = err
	}
	return first
}

// DirtyKeys lists the keys of unsent entries (for recovery checks).
func (c *Cache) DirtyKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k, e := range c.entries {
		if e.Dirty && !e.doomed {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
