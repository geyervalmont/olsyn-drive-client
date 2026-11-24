package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// Options configure Simple.
type Options struct {
	// Parallel is how many parts of one upload go at once. Default 4.
	Parallel int
	// Attempts per part or download chunk before giving up on a failure
	// that should pass (the caller retries later). Default 6.
	Attempts int
	// Backoff between attempts.
	Backoff driveapi.Backoff
	// ChunkSize for downloads (at most the drive's 64 MiB range limit).
	// Default 8 MiB.
	ChunkSize int64
	// OnEvent receives progress; it must not block for long.
	OnEvent func(Event)
	Logger  *slog.Logger
}

// Simple is the plain Engine.
type Simple struct {
	api  *driveapi.Client
	opts Options
	log  *slog.Logger
	gate *gate
	up   limiter
	down limiter
}

var _ Engine = (*Simple)(nil)

// NewSimple returns an engine using api.
func NewSimple(api *driveapi.Client, opts Options) *Simple {
	if opts.Parallel <= 0 {
		opts.Parallel = 4
	}
	if opts.Attempts <= 0 {
		opts.Attempts = 6
	}
	if opts.ChunkSize <= 0 || opts.ChunkSize > 64<<20 {
		opts.ChunkSize = 8 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Simple{api: api, opts: opts, log: opts.Logger, gate: newGate()}
}

// Pause implements Engine.
func (s *Simple) Pause() { s.gate.pause() }

// Resume implements Engine.
func (s *Simple) Resume() { s.gate.resume() }

// Paused implements Engine.
func (s *Simple) Paused() bool { return s.gate.isPaused() }

// SetBandwidth implements Engine.
func (s *Simple) SetBandwidth(up, down int64) {
	s.up.set(up)
	s.down.set(down)
}

func (s *Simple) emit(e Event) {
	if s.opts.OnEvent != nil {
		e.At = time.Now()
		s.opts.OnEvent(e)
	}
}

// errRestart means the upload session is gone: start a new one.
var errRestart = errors.New("transfer: upload session gone")

// ErrChecksumMismatch means the drive received bytes whose SHA-256 differs
// from this PC's, on the first attempt and again on a fresh upload. The
// error also carries the drive's checksum_mismatch refusal. Hold the change:
// retrying blindly would not help.
var ErrChecksumMismatch = errors.New("transfer: the drive received different bytes from those sent, twice")

// Upload implements Engine. Every upload carries the content's SHA-256 at
// completion: the drive checks small files at once and larger ones in a
// later pass (see FS checksum handling in vfs).
func (s *Simple) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	ev := Event{Direction: Up, TransferID: req.TransferID, Name: req.Name, Total: req.Size}
	var sum string
	if req.PreCheck {
		h, err := hashReaderAt(ctx, req.Src, req.Size)
		if err != nil {
			return nil, fmt.Errorf("transfer: hash %s: %w", req.TransferID, err)
		}
		sum = h
	}
	if req.PreCheck {
		res, err := s.api.CheckUploads(ctx, []driveapi.CheckFile{{ParentID: req.ParentID, Name: req.Name, Size: req.Size,
			Fingerprint: req.Fingerprint, SHA256: sum}}, false)
		if err != nil {
			return nil, err
		}
		if res[0].State == driveapi.CheckPresent {
			n, err := s.api.GetNode(ctx, res[0].ID)
			if err != nil {
				return nil, err
			}
			ev.Kind = EventSkipped
			s.emit(ev)
			return &UploadResult{Node: &n.Node, Skipped: true}, nil
		}
	}
	ev.Kind = EventStarted
	s.emit(ev)

	uploadID := req.UploadID
	restarts, mismatches := 0, 0
	for {
		res, err := s.uploadOnce(ctx, req, uploadID, sum, &ev)
		if errors.Is(err, ErrChecksumMismatch) && mismatches == 0 {
			// the drive got other bytes than ours: send it all again once,
			// hashing again too (the first hash may have been the bad read)
			mismatches++
			s.log.WarnContext(ctx, "drive reported a checksum mismatch, uploading again", "transfer", req.TransferID)
			uploadID, sum = "", ""
			continue
		}
		if errors.Is(err, errRestart) && restarts < 2 {
			restarts++
			s.log.InfoContext(ctx, "upload session gone, starting again", "transfer", req.TransferID)
			uploadID = ""
			continue
		}
		if err != nil {
			ev.Kind, ev.Err = EventFailed, err
			s.emit(ev)
			return nil, err
		}
		ev.Kind, ev.Bytes = EventDone, req.Size
		s.emit(ev)
		return res, nil
	}
}

func (s *Simple) uploadOnce(ctx context.Context, req UploadRequest, uploadID, sum string, ev *Event) (*UploadResult, error) {
	var tk *driveapi.UploadTicket
	start := driveapi.StartUpload{ParentID: req.ParentID, Name: req.Name, Size: req.Size, Mime: req.Mime,
		Conflict: req.Conflict, BaseVersionID: req.BaseVersionID, Fingerprint: req.Fingerprint}
	if !req.ModifiedAt.IsZero() {
		start.ModifiedAt = driveapi.NewTime(req.ModifiedAt)
	}
	complete := driveapi.CompleteUpload{}
	if !req.ModifiedAt.IsZero() {
		complete.ModifiedAt = driveapi.NewTime(req.ModifiedAt)
	}
	// The SHA-256 is worked out in one sequential pass beside the part
	// uploads (they read the same local file, in parallel and out of
	// order), so it costs no extra time and holds 1 MiB, never the file.
	// A resumed upload whose parts were sent before is hashed the same way.
	hctx, hcancel := context.WithCancel(ctx)
	hash := startHash(hctx, req.Src, req.Size, sum)
	defer func() {
		hcancel()
		<-hash.done
	}()
	if uploadID != "" {
		// A session from before a crash or restart: is it still open?
		_, err := s.api.GetUpload(ctx, uploadID)
		switch {
		case err == nil:
			// open: starting again with the same fingerprint hands it back
		case driveapi.IsNotFound(err) || driveapi.IsUploadGone(err):
			// finished or reaped: completing again returns the file if it finished
			if complete.SHA256, err = hash.wait(ctx); err != nil {
				return nil, fmt.Errorf("transfer: hash %s: %w", req.TransferID, err)
			}
			node, cerr := s.api.Complete(ctx, uploadID, complete)
			if cerr == nil {
				return &UploadResult{Node: node, Resumed: true}, nil
			}
			if !driveapi.IsNotFound(cerr) && !driveapi.IsUploadGone(cerr) {
				return nil, cerr
			}
		default:
			return nil, err
		}
	}
	err := driveapi.Retry(ctx, s.opts.Backoff, s.opts.Attempts, func(ctx context.Context) error {
		var err error
		tk, err = s.api.StartUpload(ctx, start)
		return err
	})
	if err != nil {
		return nil, err
	}
	if req.OnSession != nil {
		req.OnSession(tk.UploadID)
	}
	var sent atomic.Int64
	var emitMu sync.Mutex
	var lastEmit time.Time
	count := func(n int) {
		v := sent.Add(int64(n))
		emitMu.Lock()
		defer emitMu.Unlock()
		if time.Since(lastEmit) < 200*time.Millisecond && v < req.Size {
			return
		}
		lastEmit = time.Now()
		ev.Kind, ev.Bytes = EventProgress, v
		s.emit(*ev)
	}
	if tk.Method == driveapi.MethodSingle {
		err = s.putSingle(ctx, req, start, tk, count)
	} else {
		err = s.putParts(ctx, req, tk, count)
	}
	if err != nil {
		return nil, err
	}
	if complete.SHA256, err = hash.wait(ctx); err != nil {
		return nil, fmt.Errorf("transfer: hash %s: %w", req.TransferID, err)
	}
	var node *driveapi.Node
	for tries := 0; ; tries++ {
		err = driveapi.Retry(ctx, s.opts.Backoff, s.opts.Attempts, func(ctx context.Context) error {
			var err error
			node, err = s.api.Complete(ctx, tk.UploadID, complete)
			return err
		})
		if err == nil {
			break
		}
		var e *driveapi.Error
		switch {
		case errors.As(err, &e) && e.Code == driveapi.CodeUploadIncomplete && tries < 2 && tk.Method == driveapi.MethodMultipart:
			// storage is missing parts: send what it lacks and complete again
			st, serr := s.api.GetUpload(ctx, tk.UploadID)
			if serr != nil {
				return nil, serr
			}
			tk.Uploaded, tk.Parts = st.Uploaded, nil
			if err := s.putParts(ctx, req, tk, count); err != nil {
				return nil, err
			}
		case errors.As(err, &e) && e.Code == driveapi.CodeChecksumMismatch:
			// nothing was published: drop the session so a fresh one starts
			if aerr := s.api.AbortUpload(ctx, tk.UploadID); aerr != nil && !driveapi.IsNotFound(aerr) {
				s.log.WarnContext(ctx, "abort mismatched upload", "transfer", req.TransferID, "code", errCodeOf(aerr))
			}
			return nil, fmt.Errorf("%w: %w", ErrChecksumMismatch, err)
		case driveapi.IsUploadGone(err):
			return nil, errRestart
		default:
			return nil, err
		}
	}
	return &UploadResult{Node: node, Resumed: tk.Resumed, Sent: sent.Load()}, nil
}

func (s *Simple) putSingle(ctx context.Context, req UploadRequest, start driveapi.StartUpload, tk *driveapi.UploadTicket, count func(int)) error {
	url, headers := tk.URL, tk.Headers
	var last error
	for attempt := 0; attempt < s.opts.Attempts; attempt++ {
		body := &throttled{ctx: ctx, r: io.NewSectionReader(req.Src, 0, req.Size), g: s.gate, l: &s.up, count: count}
		_, err := s.api.PutPart(ctx, url, headers, body, req.Size)
		switch {
		case err == nil, driveapi.IsAlreadyStored(err):
			return nil
		case driveapi.IsStorageURLExpired(err):
			// a fresh URL: the same fingerprint hands back this upload
			ntk, serr := s.api.StartUpload(ctx, start)
			if serr != nil {
				return serr
			}
			if ntk.UploadID != tk.UploadID {
				return errRestart
			}
			url, headers = ntk.URL, ntk.Headers
		case driveapi.IsUploadGone(err):
			return errRestart
		case !driveapi.IsRetryable(err) && !driveapi.IsNetwork(err):
			return err
		default:
			s.emit(Event{Kind: EventWaiting, Direction: Up, TransferID: req.TransferID, Name: req.Name, Err: err})
			if serr := sleep(ctx, s.opts.Backoff.Delay(attempt, err)); serr != nil {
				return serr
			}
		}
		last = err
	}
	return last
}

func (s *Simple) putParts(ctx context.Context, req UploadRequest, tk *driveapi.UploadTicket, count func(int)) error {
	have := map[int]bool{}
	for _, p := range tk.Uploaded {
		have[p.Number] = true
		count(int(p.Size))
	}
	var todo []int
	for n := 1; n <= tk.PartCount; n++ {
		if !have[n] {
			todo = append(todo, n)
		}
	}
	urls := &partURLs{api: s.api, uploadID: tk.UploadID, urls: map[int]string{}}
	for _, p := range tk.Parts {
		urls.urls[p.Number] = p.URL
	}
	urls.todo = todo

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}
	for i := 0; i < s.opts.Parallel && i < len(todo); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range work {
				if err := s.putPart(ctx, req, tk, urls, n, count); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
feed:
	for _, n := range todo {
		select {
		case work <- n:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (s *Simple) putPart(ctx context.Context, req UploadRequest, tk *driveapi.UploadTicket, urls *partURLs, n int, count func(int)) error {
	off := int64(n-1) * tk.PartSize
	size := min(tk.PartSize, req.Size-off)
	var last error
	for attempt := 0; attempt < s.opts.Attempts; attempt++ {
		url, err := urls.get(ctx, n)
		if err != nil {
			if driveapi.IsNotFound(err) || driveapi.IsUploadGone(err) {
				return errRestart
			}
			return err
		}
		var partSent int64
		body := &throttled{ctx: ctx, r: io.NewSectionReader(req.Src, off, size), g: s.gate, l: &s.up,
			count: func(k int) { partSent += int64(k); count(k) }}
		_, err = s.api.PutPart(ctx, url, nil, body, size)
		if err == nil {
			return nil
		}
		count(-int(partSent)) // that part's bytes did not arrive
		switch {
		case driveapi.IsStorageURLExpired(err):
			urls.forget(n)
		case driveapi.IsUploadGone(err):
			return errRestart
		case ctx.Err() != nil:
			return ctx.Err()
		case !driveapi.IsRetryable(err) && !driveapi.IsNetwork(err):
			return err
		default:
			s.emit(Event{Kind: EventWaiting, Direction: Up, TransferID: req.TransferID, Name: req.Name, Err: err})
			if serr := sleep(ctx, s.opts.Backoff.Delay(attempt, err)); serr != nil {
				return serr
			}
		}
		last = err
	}
	return last
}

// partURLs hands out presigned part URLs, asking the drive for fresh ones
// (up to 100 at a time, for the parts still to send) when missing.
type partURLs struct {
	api      *driveapi.Client
	uploadID string
	mu       sync.Mutex
	urls     map[int]string
	todo     []int
}

func (p *partURLs) get(ctx context.Context, n int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.urls[n]; ok {
		return u, nil
	}
	batch := []int{n}
	for _, m := range p.todo {
		if len(batch) == driveapi.UploadPartsMax {
			break
		}
		if _, ok := p.urls[m]; !ok && m > n {
			batch = append(batch, m)
		}
	}
	sort.Ints(batch)
	var parts []driveapi.PartURL
	err := driveapi.Retry(ctx, driveapi.Backoff{}, 4, func(ctx context.Context) error {
		var err error
		parts, err = p.api.PartURLs(ctx, p.uploadID, batch)
		return err
	})
	if err != nil {
		return "", err
	}
	for _, pu := range parts {
		p.urls[pu.Number] = pu.URL
	}
	u, ok := p.urls[n]
	if !ok {
		return "", fmt.Errorf("transfer: drive gave no URL for part %d", n)
	}
	return u, nil
}

func (p *partURLs) forget(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// an expired URL means the others from the same batch expired too
	p.urls = map[int]string{}
	_ = n
}

// Download implements Engine.
func (s *Simple) Download(ctx context.Context, req DownloadRequest) error {
	ev := Event{Direction: Down, TransferID: req.TransferID, Name: req.Name, Total: req.Length, Kind: EventStarted}
	s.emit(ev)
	off, end := req.Offset, req.Offset+req.Length
	attempt := 0
	for off < end {
		n := min(s.opts.ChunkSize, end-off)
		got, err := s.downloadChunk(ctx, req, off, n)
		off += got
		ev.Bytes += got
		if err == nil {
			attempt = 0
			ev.Kind = EventProgress
			s.emit(ev)
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !driveapi.IsRetryable(err) || attempt+1 >= s.opts.Attempts {
			ev.Kind, ev.Err = EventFailed, err
			s.emit(ev)
			return err
		}
		if got == 0 {
			attempt++
		}
		if serr := sleep(ctx, s.opts.Backoff.Delay(attempt, err)); serr != nil {
			return serr
		}
	}
	ev.Kind = EventDone
	s.emit(ev)
	return nil
}

func (s *Simple) downloadChunk(ctx context.Context, req DownloadRequest, off, n int64) (int64, error) {
	d, err := s.api.Download(ctx, req.NodeID, req.VersionID, off, n)
	if err != nil {
		return 0, err
	}
	defer d.Body.Close()
	r := &throttled{ctx: ctx, r: d.Body, g: s.gate, l: &s.down}
	buf := make([]byte, chunk)
	var got int64
	for got < d.Length {
		k, rerr := r.Read(buf)
		if k > 0 {
			if _, werr := req.Dst.WriteAt(buf[:k], off+got); werr != nil {
				return got, fmt.Errorf("transfer: store downloaded bytes: %w", werr)
			}
			got += int64(k)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return got, rerr
		}
	}
	if got < n && off+got < d.Total {
		return got, &driveapi.Error{Op: "download", Err: io.ErrUnexpectedEOF}
	}
	return got, nil
}

// pendingHash is a SHA-256 being worked out in the background.
type pendingHash struct {
	done chan struct{}
	sum  string
	err  error
}

// startHash hashes src in a goroutine (or hands back known at once). The
// goroutine stops when ctx ends.
func startHash(ctx context.Context, src io.ReaderAt, size int64, known string) *pendingHash {
	p := &pendingHash{done: make(chan struct{})}
	if known != "" {
		p.sum = known
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		p.sum, p.err = hashReaderAt(ctx, src, size)
	}()
	return p
}

// wait returns the hash once it is ready.
func (p *pendingHash) wait(ctx context.Context) (string, error) {
	select {
	case <-p.done:
		return p.sum, p.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// errCodeOf names an error for a log line (a code, never content).
func errCodeOf(err error) string {
	if e, ok := driveapi.AsError(err); ok && e.Code != "" {
		return e.Code
	}
	return "error"
}

func hashReaderAt(ctx context.Context, r io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := min(int64(len(buf)), size-off)
		k, err := r.ReadAt(buf[:n], off)
		h.Write(buf[:k])
		off += int64(k)
		if err != nil && !(errors.Is(err, io.EOF) && off == size) {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
