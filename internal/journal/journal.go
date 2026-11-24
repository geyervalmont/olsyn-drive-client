// Package journal is the durable, ordered queue of local changes not yet
// on the drive: folder creates, content uploads, renames and moves, deletes
// and modified times, each with the upload session state needed to resume.
//
// The journal is an append-only log of small records (length, CRC-32C,
// payload) with the live operations kept in memory. Writing and making
// durable are separate steps so that many changes can share one fsync
// (group commit):
//
//   - Write hands a record to the operating system before it returns, so a
//     crash of this process loses nothing.
//   - Sync waits until everything written so far is on the device. Callers
//     that arrive while an fsync is running wait for the next one, which
//     covers all of them: N concurrent savers cost about two fsyncs, not N.
//   - Append is Write followed by Sync.
//
// The file system writes a saved file's change in Cleanup (the app's
// CloseHandle) and waits for Sync in Close, which WinFsp delivers after
// the app has moved on; a file whose Close returned survives a power loss.
// Update and Done are written but not waited for: if a power loss drops
// them, the operation replays, and every operation is idempotent on replay
// (uploads resume by upload id and fingerprint and a completed upload
// completes again to the same file; renames and modified times are
// absolute; deletes of something already gone succeed; folder creates
// adopt an existing folder of that name).
//
// A record cut short by a crash (a torn tail) fails its checksum and is
// dropped when the journal is opened; records before it are intact because
// the log is never rewritten in place. When most of the file is dead
// records it is compacted: the live operations are written to a new file,
// synced, and renamed over the old one.
package journal

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kinds of operation.
const (
	OpMkdir   = "mkdir"
	OpUpload  = "upload"
	OpRename  = "rename" // rename and/or move
	OpDelete  = "delete"
	OpSetTime = "settime"
)

// States of an operation.
const (
	StatePending = "pending" // waiting or in progress
	StateHeld    = "held"    // refused by the drive: waits for the person
)

// Op is one queued change. NodeID is the index's local id; the drive ids
// are looked up from the index when the op runs (they may not exist yet
// when it is queued).
type Op struct {
	Seq    uint64    `json:"seq"`
	Kind   string    `json:"kind"`
	NodeID string    `json:"node"`
	State  string    `json:"state"`
	Queued time.Time `json:"queued"`

	// What the change was when queued (also enough to rebuild a local
	// entry lost in a crash between journal and index).
	ParentID string    `json:"parent,omitempty"`
	Name     string    `json:"name,omitempty"`
	IsDir    bool      `json:"dir,omitempty"`
	MTime    time.Time `json:"mtime,omitzero"`

	// Renames: where the node was before (its folder's local id and its
	// name), so a change that takes that name waits for this one.
	FromParent string `json:"from_parent,omitempty"`
	FromName   string `json:"from_name,omitempty"`

	// WaitFor lists earlier operations this one must not overtake (a
	// folder's delete waits for moves out of it to reach the drive).
	WaitFor []uint64 `json:"wait,omitempty"`

	// Uploads.
	ContentKey    string `json:"ckey,omitempty"`
	Size          int64  `json:"size,omitempty"`
	BaseVersionID string `json:"base,omitempty"`
	Fingerprint   string `json:"fp,omitempty"`
	UploadID      string `json:"upload,omitempty"` // the drive's upload session, once started
	// CRC is the CRC-32C of a small new file's content when it was queued,
	// before its data was fsynced: after a power loss, content the cache
	// had to adopt is checked against it rather than sent damaged.
	CRC uint32 `json:"crc,omitempty"`

	// ServerID is the drive id of the node when it was known at queue time
	// (deletes of nodes no longer in the index), or, for a folder create,
	// the folder made on the drive for a folder deleted here meanwhile
	// (still to be sent to the trash).
	ServerID string `json:"sid,omitempty"`

	// Retry and refusal bookkeeping.
	Attempts  int       `json:"attempts,omitempty"`
	LastError string    `json:"err,omitempty"` // a code, never file content
	HeldCode  string    `json:"held_code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	NotBefore time.Time `json:"not_before,omitzero"`
}

// ErrNotFound is returned for an unknown sequence number.
var ErrNotFound = errors.New("journal: no such operation")

// ErrClosed is returned after Close, or after a failed write that could not
// be undone (the journal refuses further writes rather than risk a gap).
var ErrClosed = errors.New("journal: closed")

// Record types.
const (
	recPut  byte = 1 // payload: JSON Op
	recDel  byte = 2 // payload: 8-byte seq
	recNext byte = 3 // payload: 8-byte next sequence number (written by compaction)
)

const (
	headerLen = 8       // 4-byte length + 4-byte CRC-32C
	maxRecord = 1 << 20 // an op is a few hundred bytes; anything larger is corruption
	// compaction: when the file is past compactMin and less than a quarter live
	compactMin = 4 << 20
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Stats counts what the journal did; for tests and diagnostics.
type Stats struct {
	Writes      uint64 // records written
	Syncs       uint64 // fsyncs of the log
	Compactions uint64
	Size        int64 // bytes in the log file
	Live        int   // queued operations
	TornBytes   int64 // bytes dropped from the tail when the journal was opened
	// Written and Synced are positions (records since open): every record
	// up to Synced is on the device.
	Written, Synced uint64
}

// Options tune a journal.
type Options struct {
	// GroupDelay is the shortest gap between two fsyncs: an fsync starts no
	// sooner than this after the previous one ended, so that while saves
	// stream in each fsync covers many of them (on some file systems an
	// fsync also stalls other writers, so back-to-back fsyncs would starve
	// the app). A Sync after a quiet spell does not wait. Measured on macOS
	// (TestManySmallSavesAreFastAndDurable): 4 ms gives about 9 files per
	// fsync and 1 ms a file for the app, against 8 ms a file with an fsync
	// each. Default 4 ms; negative means no gap.
	GroupDelay time.Duration
}

// Journal is the queue.
type Journal struct {
	path       string
	groupDelay time.Duration
	lastSync   time.Time // when the last fsync ended; guarded by syncSem

	// syncSem (capacity 1) is held while fsyncing or compacting, so there
	// is one fsync at a time and the waiters behind it share the next.
	syncSem chan struct{}

	mu      sync.Mutex
	f       *os.File
	broken  error // set when the log may hold a partial record we could not remove
	ops     map[uint64]Op
	sizes   map[uint64]int64 // encoded size of each live op's latest record
	order   []uint64         // seqs in append order; may hold seqs already done
	nextSeq uint64
	size    int64  // file size
	live    int64  // bytes of the latest record of each live op
	written uint64 // records written since open (a position, not an offset)
	synced  uint64 // records known to be on the device
	stats   Stats
}

// Open opens (or creates) the journal at path with default options.
func Open(path string) (*Journal, error) { return OpenWith(path, Options{}) }

// OpenWith opens (or creates) the journal at path, replaying it and
// dropping a torn tail.
func OpenWith(path string, opts Options) (*Journal, error) {
	if opts.GroupDelay == 0 {
		opts.GroupDelay = 4 * time.Millisecond
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	j := &Journal{path: path, groupDelay: max(opts.GroupDelay, 0), f: f, syncSem: make(chan struct{}, 1),
		ops: map[uint64]Op{}, sizes: map[uint64]int64{}, nextSeq: 1}
	good, err := j.replay(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: stat %s: %w", path, err)
	}
	if st.Size() > good {
		j.stats.TornBytes = st.Size() - good
		if err := f.Truncate(good); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("journal: drop torn tail of %s: %w", path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("journal: sync %s: %w", path, err)
		}
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: seek %s: %w", path, err)
	}
	j.size = good
	return j, nil
}

// replay reads records until the end or the first bad one and returns the
// offset just past the last good record.
func (j *Journal) replay(f *os.File) (int64, error) {
	r := bufio.NewReaderSize(f, 256<<10)
	var off int64
	var hdr [headerLen]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return off, nil // end, or a header cut short
		}
		n := binary.LittleEndian.Uint32(hdr[0:4])
		sum := binary.LittleEndian.Uint32(hdr[4:8])
		if n == 0 || n > maxRecord {
			return off, nil
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return off, nil
		}
		if crc32.Checksum(payload, castagnoli) != sum {
			return off, nil
		}
		if err := j.apply(payload, int64(headerLen)+int64(n)); err != nil {
			return 0, fmt.Errorf("journal: record at %d of %s: %w", off, j.path, err)
		}
		off += int64(headerLen) + int64(n)
	}
}

// apply replays one record into memory.
func (j *Journal) apply(payload []byte, recLen int64) error {
	switch payload[0] {
	case recPut:
		var op Op
		if err := json.Unmarshal(payload[1:], &op); err != nil {
			return err
		}
		j.put(op, recLen)
	case recDel:
		if len(payload) != 9 {
			return errors.New("bad delete record")
		}
		seq := binary.LittleEndian.Uint64(payload[1:])
		j.del(seq)
		j.nextSeq = max(j.nextSeq, seq+1)
	case recNext:
		if len(payload) != 9 {
			return errors.New("bad sequence record")
		}
		j.nextSeq = max(j.nextSeq, binary.LittleEndian.Uint64(payload[1:]))
	default:
		return fmt.Errorf("unknown record type %d", payload[0])
	}
	return nil
}

func (j *Journal) put(op Op, recLen int64) {
	if _, ok := j.ops[op.Seq]; !ok {
		j.order = append(j.order, op.Seq)
	}
	j.live += recLen - j.sizes[op.Seq]
	j.ops[op.Seq], j.sizes[op.Seq] = op, recLen
	j.nextSeq = max(j.nextSeq, op.Seq+1)
}

func (j *Journal) del(seq uint64) {
	if _, ok := j.ops[seq]; !ok {
		return
	}
	j.live -= j.sizes[seq]
	delete(j.ops, seq)
	delete(j.sizes, seq)
	if len(j.order) > 64 && len(j.order) > 2*len(j.ops) {
		kept := j.order[:0]
		for _, s := range j.order {
			if _, ok := j.ops[s]; ok {
				kept = append(kept, s)
			}
		}
		j.order = kept
	}
}

func encode(typ byte, body []byte) []byte {
	rec := make([]byte, headerLen+1+len(body))
	rec[headerLen] = typ
	copy(rec[headerLen+1:], body)
	binary.LittleEndian.PutUint32(rec[0:4], uint32(1+len(body)))
	binary.LittleEndian.PutUint32(rec[4:8], crc32.Checksum(rec[headerLen:], castagnoli))
	return rec
}

func encodeOp(op Op) ([]byte, error) {
	body, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	return encode(recPut, body), nil
}

func encodeSeq(typ byte, seq uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seq)
	return encode(typ, b[:])
}

// writeLocked appends one record to the file. On a failed write it cuts the
// file back so no partial record sits before later ones.
func (j *Journal) writeLocked(rec []byte) error {
	if j.broken != nil {
		return j.broken
	}
	if j.f == nil {
		return ErrClosed
	}
	if _, err := j.f.Write(rec); err != nil {
		if terr := j.f.Truncate(j.size); terr != nil {
			j.broken = fmt.Errorf("%w: write failed (%v) and could not be undone (%v)", ErrClosed, err, terr)
		} else if _, serr := j.f.Seek(j.size, io.SeekStart); serr != nil {
			j.broken = fmt.Errorf("%w: write failed (%v) and could not seek (%v)", ErrClosed, err, serr)
		}
		return err
	}
	j.size += int64(len(rec))
	j.written++
	j.stats.Writes++
	return nil
}

// Write queues op and hands it to the operating system: it survives a crash
// of this process at once, and a power loss after the next Sync.
func (j *Journal) Write(op Op) (Op, error) {
	if op.State == "" {
		op.State = StatePending
	}
	if op.Queued.IsZero() {
		op.Queued = time.Now().UTC()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	op.Seq = j.nextSeq
	rec, err := encodeOp(op)
	if err != nil {
		return Op{}, fmt.Errorf("journal: append %s: %w", op.Kind, err)
	}
	if err := j.writeLocked(rec); err != nil {
		return Op{}, fmt.Errorf("journal: append %s: %w", op.Kind, err)
	}
	j.put(op, int64(len(rec)))
	return op, nil
}

// Append queues op durably (Write, then Sync) and returns it with its
// sequence number.
func (j *Journal) Append(op Op) (Op, error) {
	op, err := j.Write(op)
	if err != nil {
		return Op{}, err
	}
	if err := j.Sync(context.Background()); err != nil {
		return Op{}, err
	}
	return op, nil
}

// Sync returns once every record written before the call is on the
// device. Concurrent callers share fsyncs.
func (j *Journal) Sync(ctx context.Context) error {
	j.mu.Lock()
	target := j.written
	done := j.synced >= target
	j.mu.Unlock()
	if done {
		return nil
	}
	select {
	case j.syncSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("journal: sync: %w", ctx.Err())
	}
	defer func() { <-j.syncSem }()
	j.mu.Lock()
	covered := j.synced >= target
	j.mu.Unlock()
	if covered {
		return nil // the fsync we waited behind covered us
	}
	if gap := j.groupDelay - time.Since(j.lastSync); gap > 0 {
		// saves are streaming in: gather more for this fsync
		t := time.NewTimer(gap)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("journal: sync: %w", ctx.Err())
		}
	}
	j.mu.Lock()
	f, upTo, broken := j.f, j.written, j.broken
	j.mu.Unlock()
	if broken != nil {
		return broken
	}
	if f == nil {
		return ErrClosed
	}
	err := f.Sync()
	j.lastSync = time.Now()
	if err != nil {
		return fmt.Errorf("journal: sync %s: %w", j.path, err)
	}
	j.mu.Lock()
	j.synced = max(j.synced, upTo)
	j.stats.Syncs++
	j.mu.Unlock()
	return nil
}

// Update rewrites an operation (state, upload id, attempts, …). It is
// written, not waited for (see the package comment).
func (j *Journal) Update(op Op) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.ops[op.Seq]; !ok {
		return fmt.Errorf("journal: update %d: %w", op.Seq, ErrNotFound)
	}
	rec, err := encodeOp(op)
	if err != nil {
		return fmt.Errorf("journal: update %d: %w", op.Seq, err)
	}
	if err := j.writeLocked(rec); err != nil {
		return fmt.Errorf("journal: update %d: %w", op.Seq, err)
	}
	j.put(op, int64(len(rec)))
	return nil
}

// Done removes a finished (or abandoned) operation. Removing one that is
// already gone is not an error.
func (j *Journal) Done(seq uint64) error {
	j.mu.Lock()
	if _, ok := j.ops[seq]; !ok {
		j.mu.Unlock()
		return nil
	}
	if err := j.writeLocked(encodeSeq(recDel, seq)); err != nil {
		j.mu.Unlock()
		return fmt.Errorf("journal: done %d: %w", seq, err)
	}
	j.del(seq)
	compact := j.size > compactMin && j.live*4 < j.size
	j.mu.Unlock()
	if compact {
		if err := j.compact(); err != nil {
			return fmt.Errorf("journal: compact: %w", err)
		}
	}
	return nil
}

// compact rewrites the log with only the live operations.
func (j *Journal) compact() error {
	j.syncSem <- struct{}{}
	defer func() { <-j.syncSem }()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil || j.broken != nil || !(j.size > compactMin && j.live*4 < j.size) {
		return nil
	}
	tmp := j.path + ".compact"
	nf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(nf, 256<<10)
	var size int64
	sizes := make(map[uint64]int64, len(j.ops))
	order := make([]uint64, 0, len(j.ops))
	write := func(rec []byte) error {
		size += int64(len(rec))
		_, err := w.Write(rec)
		return err
	}
	err = write(encodeSeq(recNext, j.nextSeq))
	for _, seq := range j.order {
		op, ok := j.ops[seq]
		if !ok || err != nil {
			continue
		}
		var rec []byte
		if rec, err = encodeOp(op); err == nil {
			err = write(rec)
			sizes[seq] = int64(len(rec))
			order = append(order, seq)
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = nf.Sync()
	}
	if cerr := nf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Windows cannot rename over an open file: close ours first.
	if err := j.f.Close(); err != nil {
		j.broken = fmt.Errorf("%w: close for compaction: %v", ErrClosed, err)
		return err
	}
	j.f = nil
	if err := os.Rename(tmp, j.path); err != nil {
		// the old log is still complete: reopen it
		_ = os.Remove(tmp)
		return j.reopenLocked(j.size, err)
	}
	if err := syncDir(filepath.Dir(j.path)); err != nil {
		// the rename is done; a lost directory entry update means the old
		// complete log comes back after a power loss, which is still correct
		_ = err
	}
	if err := j.reopenLocked(size, nil); err != nil {
		return err
	}
	j.sizes, j.order, j.live = sizes, order, size
	j.synced = j.written // everything is in the synced new file
	j.stats.Compactions++
	return nil
}

func (j *Journal) reopenLocked(size int64, cause error) error {
	f, err := os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err == nil {
		_, err = f.Seek(size, io.SeekStart)
	}
	if err != nil {
		if f != nil {
			_ = f.Close()
		}
		j.broken = fmt.Errorf("%w: reopen after compaction: %v", ErrClosed, err)
		return err
	}
	j.f, j.size = f, size
	return cause
}

// Get reads one operation.
func (j *Journal) Get(seq uint64) (Op, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	op, ok := j.ops[seq]
	if !ok {
		return Op{}, ErrNotFound
	}
	return op, nil
}

// List returns every queued operation in order.
func (j *Journal) List() ([]Op, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Op, 0, len(j.ops))
	for _, seq := range j.order {
		if op, ok := j.ops[seq]; ok {
			out = append(out, op)
		}
	}
	return out, nil
}

// Len is the number of queued operations.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.ops)
}

// Stats reports counters.
func (j *Journal) Stats() Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.stats
	s.Size, s.Live, s.Written, s.Synced = j.size, len(j.ops), j.written, j.synced
	return s
}

// Close syncs and closes the file.
func (j *Journal) Close() error {
	serr := j.Sync(context.Background())
	j.syncSem <- struct{}{}
	defer func() { <-j.syncSem }()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	if serr != nil && !errors.Is(serr, ErrClosed) {
		return serr
	}
	if err != nil {
		return fmt.Errorf("journal: close %s: %w", j.path, err)
	}
	return nil
}

// Abandon closes the file without syncing, as a crashed process would
// leave it. For tests.
func (j *Journal) Abandon() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		_ = j.f.Close()
		j.f = nil
	}
}
