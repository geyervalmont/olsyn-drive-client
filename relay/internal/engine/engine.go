// Package engine moves one local file into the drive, safely: start (or
// resume, by fingerprint), send parts in parallel straight to storage while
// hashing the file once in order, complete, then verify the drive's own
// hash of what it stored. Every step is recorded (package state) before the
// step it enables, so a crash anywhere resumes without loss or duplicates.
//
// It knows nothing about spools, `send` or mounts: callers give it a
// destination and a record; a spool, a one-off send or (later) a mount's
// write-back cache all use it the same way.
package engine

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/state"
)

// Outcomes of one Send.
const (
	Sent    = "sent"    // uploaded and published (verify pending or done)
	Already = "already" // the drive has this exact file
	Held    = "held"    // needs a person: name taken (policy hold), refusal, mismatch
	Failed  = "failed"  // a final error for this version of the file
)

// Conflict policies for a name the drive already has with other content.
const (
	OnConflictHold       = "hold"
	OnConflictNewVersion = "new_version"
	OnConflictKeepBoth   = "keep_both"
)

// ErrChanged: the local file changed while it was being read; the caller
// treats it as a new file (it settles again).
var ErrChanged = errors.New("the file changed while it was being sent")

// ErrCrash is returned at a crash point (tests only): the process "died".
var ErrCrash = errors.New("simulated crash")

// Engine sends files. Safe for concurrent Sends of different records.
type Engine struct {
	API   *driveapi.Client
	Store *state.Store
	Log   *slog.Logger
	// PartsInFlight is how many parts of one file are on the wire at once
	// (default 4). Memory per file is (PartsInFlight+1) × part size.
	PartsInFlight int
	// Progress is told about bytes stored (sent) or found already stored.
	Progress func(sent, skipped int64)
	// Crash, in tests, simulates the process dying at a named point: when
	// it returns true, Send stops there (after any record write before it).
	Crash func(point string) bool
	// Backoff bounds retries of one request (default 1 s to 2 min, 8 tries).
	MinBackoff, MaxBackoff time.Duration
	Tries                  int
}

func (e *Engine) crash(point string) bool { return e.Crash != nil && e.Crash(point) }

func (e *Engine) log() *slog.Logger {
	if e.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return e.Log
}

// Job is one file to send. Rec carries the destination (ParentID, Name,
// Person, Assurance), the file's identity (Rel, Size, MtimeNs,
// Fingerprint) and, after a crash, how far it got.
type Job struct {
	Abs      string
	Rec      *state.Record
	Conflict string // hold (default) | new_version | keep_both
}

// Result is a final outcome; Send returns an error instead when trying
// again later can succeed (network, drive restarting, cancelled).
type Result struct {
	Outcome string
	Reason  string
}

// Fingerprint identifies an unchanged local file to the drive: the same
// file always gives the same one, so a restart resumes its pending upload
// and a re-run's check finds it present.
func Fingerprint(rel string, size, mtimeNs int64) string {
	h := sha256.Sum256([]byte(rel))
	return fmt.Sprintf("olsyn-drive/1|%d|%d|%s", size, mtimeNs, hex.EncodeToString(h[:8]))
}

// Retry runs fn until it succeeds, fails for good, or tries run out.
func (e *Engine) Retry(ctx context.Context, what string, fn func() error) error {
	minB, maxB, tries := e.MinBackoff, e.MaxBackoff, e.Tries
	if minB == 0 {
		minB = time.Second
	}
	if maxB == 0 {
		maxB = 2 * time.Minute
	}
	if tries == 0 {
		tries = 8
	}
	wait := minB
	var err error
	for i := 0; i < tries; i++ {
		if err = fn(); err == nil || !driveapi.Retryable(err) || ctx.Err() != nil {
			return err
		}
		d := wait/2 + rand.N(wait/2+1)
		var ae *driveapi.Error
		if errors.As(err, &ae) && ae.Retry > d {
			d = ae.Retry
		}
		e.log().Info("retrying", "what", what, "in", d.Round(time.Millisecond).String(), "err", err.Error())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		wait = min(wait*2, maxB)
	}
	return err
}

func (e *Engine) put(r *state.Record, st string) error {
	r.State = st
	return e.Store.Put(r)
}

func mimeOf(name string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Send moves one file into the drive. It resumes from whatever the record
// says; it never makes a second copy of a file the drive already has.
func (e *Engine) Send(ctx context.Context, job Job) (Result, error) {
	r := job.Rec
	if r.State == state.Done || r.State == state.Verifying {
		return Result{Outcome: r.Outcome}, nil
	}
	if r.State == state.Completing && r.UploadID != "" {
		// Died after asking for complete (or before): asking again is safe.
		res, err := e.complete(ctx, r, nil)
		if err == nil || !restartable(err) {
			return res, err
		}
	}
	for attempt := 0; ; attempt++ {
		res, err := e.sendOnce(ctx, job)
		if err != nil && restartable(err) && attempt < 2 {
			// The pending upload went away (expired, reaped) or storage
			// lost parts: start again; the drive dedups by fingerprint.
			r.UploadID, r.VersionID = "", ""
			continue
		}
		return res, err
	}
}

// restartable: the upload must be started again (not just retried).
func restartable(err error) bool {
	s := driveapi.StatusOf(err)
	var ae *driveapi.Error
	incomplete := errors.As(err, &ae) && ae.Code == "upload_incomplete"
	return s == 404 || s == 410 || incomplete
}

func (e *Engine) sendOnce(ctx context.Context, job Job) (Result, error) {
	r := job.Rec
	f, err := os.Open(job.Abs)
	if err != nil {
		return Result{}, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if err := sameFile(f, r); err != nil {
		return Result{}, err
	}
	in := driveapi.UploadIn{ParentID: r.ParentID, Name: r.Name, Size: r.Size, Mime: mimeOf(r.Name), Conflict: "fail",
		Fingerprint: r.Fingerprint, OnBehalfOf: r.Person, Assurance: r.Assurance}
	switch job.Conflict {
	case OnConflictNewVersion:
		in.Conflict = "new_version"
	case OnConflictKeepBoth:
		in.Conflict = "rename"
	}
	if r.State != state.Sending {
		if err := e.put(r, state.Starting); err != nil {
			return Result{}, err
		}
	}
	var t *driveapi.Ticket
	err = e.Retry(ctx, "start "+r.Rel, func() error {
		var err error
		t, err = e.API.StartUpload(ctx, in)
		return err
	})
	if err != nil {
		return refusal(err, r.Name)
	}
	if e.crash("started") {
		return Result{}, ErrCrash
	}
	r.UploadID, r.VersionID, r.NodeID = t.UploadID, t.VersionID, t.NodeID
	if t.Name != "" {
		r.Name = t.Name
	}
	if err := e.put(r, state.Sending); err != nil {
		return Result{}, err
	}
	if e.crash("recorded") {
		return Result{}, ErrCrash
	}
	var parts []driveapi.Part
	var sum string
	if t.Method == "single" {
		sum, err = e.sendSingle(ctx, f, r, t)
	} else {
		sum, parts, err = e.sendParts(ctx, f, r, t)
	}
	if err != nil {
		return Result{}, err
	}
	if err := sameFile(f, r); err != nil {
		return Result{}, err // changed while read: the stored bytes may be a mix; never complete them
	}
	if e.crash("sent") {
		return Result{}, ErrCrash
	}
	r.SHA256 = sum
	if err := e.put(r, state.Completing); err != nil {
		return Result{}, err
	}
	if e.crash("completing") {
		return Result{}, ErrCrash
	}
	return e.complete(ctx, r, parts)
}

func (e *Engine) complete(ctx context.Context, r *state.Record, parts []driveapi.Part) (Result, error) {
	var c *driveapi.Completed
	err := e.Retry(ctx, "complete "+r.Rel, func() error {
		var err error
		c, err = e.API.Complete(ctx, r.UploadID, parts)
		return err
	})
	if err != nil {
		if restartable(err) {
			return Result{}, err
		}
		return refusal(err, r.Name)
	}
	if e.crash("completed") {
		return Result{}, ErrCrash
	}
	r.NodeID, r.VersionID, r.Outcome, r.Reason = c.ID, r.UploadID, Sent, ""
	if c.VersionID != "" {
		r.VersionID = c.VersionID
	}
	if err := e.put(r, state.Verifying); err != nil {
		return Result{}, err
	}
	return Result{Outcome: Sent}, nil
}

// refusal turns a final drive refusal into a held or failed result; a
// retryable error stays an error.
func refusal(err error, name string) (Result, error) {
	if driveapi.Retryable(err) || errors.Is(err, context.Canceled) {
		return Result{}, err
	}
	var ae *driveapi.Error
	if !errors.As(err, &ae) {
		return Result{}, err
	}
	msg := ae.Msg
	if msg == "" {
		msg = ae.Code
	}
	switch {
	case ae.Status == 409 && (ae.Code == "conflict" || strings.Contains(msg, "already exists")):
		return Result{Outcome: Held, Reason: fmt.Sprintf("a different file named %q is already there", name)}, nil
	case ae.Status == 401:
		return Result{}, fmt.Errorf("the drive no longer accepts this computer's sign-in (run olsyn-drive login): %w", err)
	case ae.Status == 403 || ae.Status == 404 || ae.Status == 423 || ae.Status == 409:
		return Result{Outcome: Held, Reason: "refused by the drive: " + msg}, nil
	default:
		return Result{Outcome: Failed, Reason: msg}, nil
	}
}

func sameFile(f *os.File, r *state.Record) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != r.Size || st.ModTime().UnixNano() != r.MtimeNs {
		return ErrChanged
	}
	return nil
}

var plainMD5 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// etagMatches: storage's ETag for a plain PUT is the MD5 of the body
// (unless encrypted with KMS, then it's opaque and can't be checked).
func etagMatches(etag string, sum []byte) bool {
	t := strings.Trim(etag, `"`)
	if !plainMD5.MatchString(t) {
		return true
	}
	return t == hex.EncodeToString(sum)
}

func (e *Engine) sendSingle(ctx context.Context, f *os.File, r *state.Record, t *driveapi.Ticket) (string, error) {
	var sum string
	err := e.Retry(ctx, "send "+r.Rel, func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		sh, mh := sha256.New(), md5.New()
		body := io.TeeReader(io.LimitReader(f, r.Size), io.MultiWriter(sh, mh))
		etag, err := e.API.Put(ctx, t.URL, t.Headers, body, r.Size)
		if driveapi.StatusOf(err) == 412 {
			// Stored already (an earlier attempt landed): read the rest to
			// finish the hash; complete checks the size.
			if _, err := io.Copy(io.Discard, body); err != nil {
				return err
			}
			sum = hex.EncodeToString(sh.Sum(nil))
			return nil
		}
		if driveapi.StatusOf(err) == 403 {
			return &driveapi.Error{Status: 410, Code: "upload_expired", Msg: "the upload URL expired"}
		}
		if err != nil {
			return err
		}
		if !etagMatches(etag, mh.Sum(nil)) {
			return &driveapi.Error{Status: 0, Code: "etag_mismatch", Msg: "storage stored different bytes than were sent"}
		}
		sum = hex.EncodeToString(sh.Sum(nil))
		if e.Progress != nil {
			e.Progress(r.Size, 0)
		}
		return nil
	})
	return sum, err
}

type partWork struct {
	n   int32
	buf []byte
	md5 []byte
}

// sendParts reads the file once, in order, hashing it, and sends the
// parts storage doesn't have yet on a bounded number of workers. Memory:
// (PartsInFlight+1) part buffers.
func (e *Engine) sendParts(ctx context.Context, f *os.File, r *state.Record, t *driveapi.Ticket) (string, []driveapi.Part, error) {
	inflight := e.PartsInFlight
	if inflight <= 0 {
		inflight = 4
	}
	ps, count := t.PartSize, t.PartCount
	if ps <= 0 || count <= 0 {
		return "", nil, fmt.Errorf("the drive gave no part layout")
	}
	have := map[int32]driveapi.Part{}
	for _, p := range t.Uploaded {
		have[p.Number] = p
	}
	urls := &urlBook{api: e.API, upload: t.UploadID, count: count, have: have, urls: map[int32]string{}}
	for _, p := range t.Parts {
		urls.urls[p.Number] = p.URL
	}
	results := make([]driveapi.Part, count)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	pool := make(chan []byte, inflight+1)
	for range inflight + 1 {
		pool <- make([]byte, ps)
	}
	work := make(chan partWork)
	var wg sync.WaitGroup
	var sentOnce atomic.Bool
	for range inflight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range work {
				err := e.Retry(ctx, fmt.Sprintf("part %d of %s", w.n, r.Rel), func() error {
					u, err := urls.get(ctx, w.n)
					if err != nil {
						return err
					}
					etag, err := e.API.Put(ctx, u, nil, bytesReader(w.buf), int64(len(w.buf)))
					if s := driveapi.StatusOf(err); s == 403 {
						urls.forget(w.n) // expired: sign again
						return &driveapi.Error{Status: 503, Code: "url_expired"}
					} else if s == 404 {
						return &driveapi.Error{Status: 410, Code: "upload_gone", Msg: "storage no longer has this upload"}
					}
					if err != nil {
						return err
					}
					if !etagMatches(etag, w.md5) {
						return &driveapi.Error{Status: 0, Code: "etag_mismatch", Msg: "storage stored different bytes than were sent"}
					}
					results[w.n-1] = driveapi.Part{Number: w.n, ETag: etag}
					return nil
				})
				if err != nil {
					cancel(err)
				} else {
					if e.Progress != nil {
						e.Progress(int64(len(w.buf)), 0)
					}
					if sentOnce.CompareAndSwap(false, true) && e.crash("part") {
						cancel(ErrCrash)
					}
				}
				pool <- w.buf[:cap(w.buf)]
			}
		}()
	}
	sh := sha256.New()
	readErr := func() error {
		defer close(work)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		for n := int32(1); n <= count; n++ {
			var buf []byte
			select {
			case buf = <-pool:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			size := ps
			if n == count {
				size = r.Size - ps*int64(count-1)
			}
			buf = buf[:size]
			if _, err := io.ReadFull(f, buf); err != nil {
				pool <- buf[:cap(buf)]
				if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
					return ErrChanged
				}
				return err
			}
			sh.Write(buf)
			if p, ok := have[n]; ok && p.Size == size {
				results[n-1] = driveapi.Part{Number: n, ETag: p.ETag}
				pool <- buf[:cap(buf)]
				if e.Progress != nil {
					e.Progress(0, size)
				}
				continue
			}
			m := md5.Sum(buf)
			select {
			case work <- partWork{n: n, buf: buf, md5: m[:]}:
			case <-ctx.Done():
				pool <- buf[:cap(buf)]
				return context.Cause(ctx)
			}
		}
		return nil
	}()
	wg.Wait()
	if cause := context.Cause(ctx); cause != nil {
		return "", nil, cause
	}
	if readErr != nil {
		return "", nil, readErr
	}
	return hex.EncodeToString(sh.Sum(nil)), results, nil
}

type bytesReaderT struct {
	b []byte
	i int
}

func bytesReader(b []byte) io.Reader { return &bytesReaderT{b: b} }

func (r *bytesReaderT) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

// urlBook hands out signed part URLs, asking the drive for the next batch
// (up to 100) when one is missing or expired.
type urlBook struct {
	mu     sync.Mutex
	api    *driveapi.Client
	upload string
	count  int32
	have   map[int32]driveapi.Part
	urls   map[int32]string
}

func (b *urlBook) forget(n int32) {
	b.mu.Lock()
	delete(b.urls, n)
	b.mu.Unlock()
}

func (b *urlBook) get(ctx context.Context, n int32) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u, ok := b.urls[n]; ok {
		return u, nil
	}
	var want []int32
	for i := n; i <= b.count && len(want) < 100; i++ {
		if _, done := b.have[i]; done {
			continue
		}
		if _, ok := b.urls[i]; !ok {
			want = append(want, i)
		}
	}
	got, err := b.api.PartURLs(ctx, b.upload, want)
	if err != nil {
		return "", err
	}
	for _, p := range got {
		b.urls[p.Number] = p.URL
	}
	u, ok := b.urls[n]
	if !ok {
		return "", fmt.Errorf("the drive signed no URL for part %d", n)
	}
	return u, nil
}

// ---- Verify ----

// Verify asks the drive for its own hash of what it stored. done: it
// matches ours. pending: not hashed yet (ask again later). Otherwise the
// result says why it can't be trusted (held, never deleted).
func (e *Engine) Verify(ctx context.Context, r *state.Record) (done, pending bool, res Result, err error) {
	if r.State == state.Done {
		return true, false, Result{Outcome: r.Outcome}, nil
	}
	var st *driveapi.UploadState
	err = e.Retry(ctx, "verify "+r.Rel, func() error {
		var err error
		st, err = e.API.RelayUpload(ctx, r.VersionID)
		return err
	})
	if driveapi.StatusOf(err) == 404 {
		r.Reason = "the drive no longer has this upload; nothing was deleted"
		return false, false, Result{Outcome: Held, Reason: r.Reason}, e.put(r, state.Held)
	}
	if err != nil {
		return false, false, Result{}, err
	}
	if st.SHA256 == nil {
		if st.Trashed || !st.Current {
			// The worker skips trashed or replaced versions: nothing will
			// ever hash it. Keep the local file and say so.
			r.Reason = "the file was replaced or trashed in the drive before it could be verified; the local copy is kept"
			return false, false, Result{Outcome: Held, Reason: r.Reason}, e.put(r, state.Held)
		}
		return false, true, Result{}, nil
	}
	if !strings.EqualFold(*st.SHA256, r.SHA256) || st.Size != r.Size {
		r.Reason = "the drive's checksum doesn't match this file; the local copy is kept"
		return false, false, Result{Outcome: Held, Reason: r.Reason}, e.put(r, state.Held)
	}
	r.Reason = ""
	return true, false, Result{Outcome: r.Outcome}, e.put(r, state.Done)
}

// HashFile is the sha256 of a local file (used when the drive has a file
// of the same name and size, to tell identical from different).
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var _ hash.Hash = sha256.New()
