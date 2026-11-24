package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// Options configure the core.
type Options struct {
	// StateDir holds per-account index and journal folders (accounts/…).
	StateDir string
	// PCName goes into conflict copy names.
	PCName     string
	HTTPClient *http.Client
	UserAgent  string
	Logger     *slog.Logger
	// AllowServer refuses servers this build may not use (release builds
	// talk only to the product's drive). Nil allows any.
	AllowServer func(serverURL string) error

	// Tuning, for tests; zero means the production default.
	PollInterval    time.Duration
	Backoff         driveapi.Backoff
	DisableStream   bool
	Workers         int
	ReleaseInterval time.Duration
}

// FS implements agent.FileSystem.
type FS struct {
	opts   Options
	log    *slog.Logger
	events chan agent.FSEvent

	mu       sync.Mutex
	run      *session // nil while stopped
	paused   bool
	bw       settings.Bandwidth
	last     accountDirs // the last session's folders, for Unsent while stopped
	mountDir string      // where the drive is mounted ("W:\"), for local paths
	hist     *history
	sum      summaryCache
}

// session is one signed-in account's running file system.
type session struct {
	sess    agent.Session
	dirs    accountDirs
	api     *driveapi.Client
	idx     *index.DB
	cache   *cache.Cache
	jr      *journal.Journal
	eng     *transfer.Simple
	vfs     *vfs.FS
	webURL  string
	ctx     context.Context // lives until Stop
	sends   *sends
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	prog    *progress
	started time.Time
	synced  int // files sent since the queue was last empty (for "finished")
}

type accountDirs struct {
	state string // index.db, journal.log, root.json
	cache string // content cache
}

// New returns a stopped core.
func New(o Options) (*FS, error) {
	if o.StateDir == "" {
		return nil, errors.New("core: StateDir is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.PCName == "" {
		o.PCName = "this PC"
	}
	return &FS{opts: o, log: o.Logger, events: make(chan agent.FSEvent, 4096), hist: newHistory(1000, 7*24*time.Hour)}, nil
}

var _ agent.FileSystem = (*FS)(nil)

// Events delivers changes for the life of the FS.
func (f *FS) Events() <-chan agent.FSEvent { return f.events }

// dirsFor names an account's folders: one per server and user, so another
// account never sees this one's unsent files or cached content.
func (f *FS) dirsFor(s agent.Session) accountDirs {
	sum := sha256.Sum256([]byte(strings.TrimRight(s.ServerURL, "/") + "|" + s.UserID))
	name := "acct-" + hex.EncodeToString(sum[:6])
	return accountDirs{state: filepath.Join(f.opts.StateDir, "accounts", name), cache: filepath.Join(s.CacheDir, name)}
}

// Start opens the account's index, cache and journal and starts the file
// system. It fails (and the agent retries) when the drive's root is not
// known yet and the drive cannot be reached to learn it.
func (f *FS) Start(ctx context.Context, s agent.Session) error {
	f.mu.Lock()
	if f.run != nil {
		f.mu.Unlock()
		return errors.New("core: already started")
	}
	f.mu.Unlock()
	if f.opts.AllowServer != nil {
		if err := f.opts.AllowServer(s.ServerURL); err != nil {
			return err
		}
	}
	api, err := newAPI(s.ServerURL, s.Token, f.opts.HTTPClient, f.opts.UserAgent, f.log)
	if err != nil {
		return err
	}
	dirs := f.dirsFor(s)
	for _, d := range []string{dirs.state, dirs.cache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("core: create %s: %w", d, err)
		}
	}
	root, webURL, err := f.rootOf(ctx, api, dirs, s.ServerURL)
	if err != nil {
		return err
	}
	r := &session{sess: s, dirs: dirs, api: api, webURL: webURL, prog: newProgress(), started: time.Now()}
	if err := f.open(r, root); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel, r.ctx = cancel, runCtx
	r.sends = newSends(f, r)
	sub, unsub := r.vfs.Subscribe(4096)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer unsub()
		f.forward(runCtx, r, sub)
	}()
	r.vfs.Start(runCtx)
	f.mu.Lock()
	f.run, f.last = r, dirs
	f.mu.Unlock()
	f.loadHistory(dirs)
	r.sends.load(runCtx)
	f.log.Info("core: session started", "server", s.ServerURL, "read_only", s.ReadOnly)
	f.signal(agent.FSEvent{StatusChanged: true})
	return nil
}

// rootOf learns the drive root (and web address) from the drive, keeping a
// copy so the drive can start offline next time.
func (f *FS) rootOf(ctx context.Context, api *driveapi.Client, dirs accountDirs, server string) (root, web string, err error) {
	type saved struct {
		RootID string `json:"root_id"`
		WebURL string `json:"web_url"`
	}
	file := filepath.Join(dirs.state, "root.json")
	var prev saved
	if b, rerr := os.ReadFile(file); rerr == nil {
		_ = json.Unmarshal(b, &prev)
	}
	mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := api.Me(mctx)
	if err != nil {
		if driveapi.IsAuthLost(err) {
			return "", "", mapDriveErr(err)
		}
		if prev.RootID != "" {
			f.log.Info("core: drive not reachable; starting from the saved root", "err", err)
			return prev.RootID, prev.WebURL, nil
		}
		return "", "", fmt.Errorf("core: read the drive's root: %w", err)
	}
	if me.RootID == "" {
		return "", "", errors.New("core: this account has no drive of its own (guest accounts are not supported yet)")
	}
	web = server
	if b, berr := api.Bootstrap(mctx); berr == nil && b.WebURL != "" {
		web = b.WebURL
	}
	if out, merr := json.Marshal(saved{RootID: me.RootID, WebURL: web}); merr == nil {
		_ = os.WriteFile(file, out, 0o600)
	}
	return me.RootID, web, nil
}

// open builds the session's parts.
func (f *FS) open(r *session, root string) (err error) {
	var closers []func() error
	defer func() {
		if err != nil {
			for i := len(closers) - 1; i >= 0; i-- {
				_ = closers[i]()
			}
		}
	}()
	if r.idx, err = index.Open(filepath.Join(r.dirs.state, "index.db")); err != nil {
		return fmt.Errorf("core: open index: %w", err)
	}
	closers = append(closers, r.idx.Close)
	if r.cache, err = cache.Open(cache.Options{Dir: r.dirs.cache, Cap: r.sess.CacheCapBytes, Logger: f.log}); err != nil {
		return fmt.Errorf("core: open cache: %w", err)
	}
	closers = append(closers, r.cache.Close)
	if r.jr, err = journal.Open(filepath.Join(r.dirs.state, "journal.log")); err != nil {
		return fmt.Errorf("core: open journal: %w", err)
	}
	closers = append(closers, r.jr.Close)
	if st := r.jr.Stats(); st.TornBytes > 0 {
		f.log.Warn("core: journal had a torn last record (crash while writing); it was dropped", "bytes", st.TornBytes)
	}
	var fsRef *vfs.FS
	var refMu sync.Mutex
	r.eng = transfer.NewSimple(r.api, transfer.Options{Backoff: f.opts.Backoff, Logger: f.log,
		OnEvent: func(ev transfer.Event) {
			refMu.Lock()
			v := fsRef
			refMu.Unlock()
			if v != nil {
				v.TransferEvent(ev)
			}
		}})
	f.mu.Lock()
	r.eng.SetBandwidth(f.bw.UpBPS, f.bw.DownBPS)
	if f.paused {
		r.eng.Pause()
	}
	f.mu.Unlock()
	v, err := vfs.New(vfs.Config{API: r.api, Engine: r.eng, Index: r.idx, Cache: r.cache, Journal: r.jr, RootID: root,
		PCName: f.opts.PCName, PollInterval: f.opts.PollInterval, Backoff: f.opts.Backoff, DisableStream: f.opts.DisableStream,
		Workers: f.opts.Workers, Logger: f.log, Release: releasePolicy(r.sess.CacheRelease), ReleaseInterval: f.opts.ReleaseInterval})
	if err != nil {
		return fmt.Errorf("core: start file system: %w", err)
	}
	refMu.Lock()
	fsRef = v
	refMu.Unlock()
	r.vfs = v
	return nil
}

// Stop flushes and closes the session.
func (f *FS) Stop(ctx context.Context) error {
	f.mu.Lock()
	r := f.run
	f.run = nil
	f.mu.Unlock()
	if r == nil {
		return nil
	}
	r.sends.stop() // sends stop at their next part and run again at the next start
	err := r.vfs.Close()
	r.cancel()
	r.wg.Wait()
	f.saveHistory(r.dirs)
	err = errors.Join(err, r.cache.Close(), r.jr.Close(), r.idx.Close())
	f.log.Info("core: session stopped")
	f.signal(agent.FSEvent{StatusChanged: true})
	if err != nil {
		return fmt.Errorf("core: stop: %w", err)
	}
	return nil
}

func (f *FS) current() *session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run
}

// VFS is the running file system (for the mount), or nil while stopped.
func (f *FS) VFS() *vfs.FS {
	if r := f.current(); r != nil {
		return r.vfs
	}
	return nil
}

// SetMountDir records where the drive is mounted, for local paths in
// transfers ("" when unmounted).
func (f *FS) SetMountDir(dir string) {
	f.mu.Lock()
	f.mountDir = dir
	f.mu.Unlock()
}

// signal delivers an event to the agent; state changes wait for room,
// progress is dropped when the agent is behind.
func (f *FS) signal(ev agent.FSEvent) {
	if ev.Transfer != nil && !ev.Transfer.Terminal() && ev.Transfer.State == agent.TransferActive {
		select {
		case f.events <- ev:
		default:
		}
		return
	}
	select {
	case f.events <- ev:
	case <-time.After(2 * time.Second):
		f.log.Warn("core: agent not reading events; one dropped")
	}
}

// forward turns vfs events into agent events.
func (f *FS) forward(ctx context.Context, r *session, sub <-chan vfs.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			f.handle(r, ev)
		}
	}
}

func (f *FS) handle(r *session, ev vfs.Event) {
	switch ev.Kind {
	case vfs.EventTransfer:
		te := ev.Transfer
		if strings.HasPrefix(te.TransferID, "b_") {
			r.sends.progress(te)
			return
		}
		seq, ok := mountSeq(te.TransferID)
		if !ok || te.Direction != transfer.Up {
			return // reads of open files and conflict copies are not listed
		}
		r.prog.note(seq, te)
		t := f.mountTransfer(r, vfs.Queued{Seq: seq, Name: te.Name, Size: te.Total, Active: true}, ev.Path)
		switch te.Kind {
		case transfer.EventWaiting, transfer.EventFailed:
			t.State = agent.TransferWaiting
			if te.Err != nil {
				t.Error = &agent.TransferError{Code: errCode(te.Err), Message: "Waiting to try again: " + driveapi.Reason(te.Err)}
			}
		case transfer.EventDone, transfer.EventSkipped:
			return // EventSynced says when the change is really done
		}
		f.signal(agent.FSEvent{Transfer: &t})
	case vfs.EventSynced:
		seq := ev.Seq
		p := r.prog.done(seq)
		if p == nil {
			return // not an upload (a folder, rename, …)
		}
		now := time.Now().UTC()
		t := f.mountTransfer(r, vfs.Queued{Seq: seq, Name: path.Base(ev.Path), Size: p.total}, ev.Path)
		t.State, t.BytesDone, t.BPS, t.ETASeconds, t.FinishedAt = agent.TransferDone, p.total, 0, nil, &now
		t.StartedAt = &p.started
		f.hist.add(t)
		f.signal(agent.FSEvent{Transfer: &t})
		f.mu.Lock()
		r.synced++
		f.mu.Unlock()
		f.maybeFinished(r)
	case vfs.EventHeld:
		t := f.heldTransfer(r, ev.Seq)
		if t == nil {
			return
		}
		r.prog.done(ev.Seq)
		f.signal(agent.FSEvent{Held: &agent.HeldChange{Op: "added", Item: *t}})
		f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "held", Title: "Couldn't save " + t.Name + " to the drive",
			Body: t.Held.Message, Action: &agent.NotificationAction{Label: "Show", Open: agent.OpenAction{Target: "ui", Path: "transfers/held"}}}})
	case vfs.EventReleased:
		state := agent.TransferCancelled
		if ev.Reason == "reupload" {
			state = agent.TransferWaiting // queued again
		}
		t := agent.Transfer{ID: mountID(ev.Seq), Direction: "up", Origin: "mount", State: state, Name: path.Base(ev.Path),
			DrivePath: ev.Path, QueuedAt: ev.At.UTC()}
		f.signal(agent.FSEvent{Held: &agent.HeldChange{Op: "removed", Item: t}})
	case vfs.EventConflict:
		copyPath := ev.Path
		f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "conflict", Title: "Saved as a conflict copy",
			Body: "Someone else changed the file while you had it open, so yours was saved beside it as " + path.Base(copyPath) + "."}})
	case vfs.EventState:
		f.signal(agent.FSEvent{StatusChanged: true})
	case vfs.EventCache:
		f.invalidate() // content left this PC: the cache figures changed
	}
}

// maybeFinished says "finished" once the queue has drained after sends.
func (f *FS) maybeFinished(r *session) {
	if len(r.vfs.Queued()) > 0 {
		return
	}
	f.mu.Lock()
	n := r.synced
	r.synced = 0
	f.mu.Unlock()
	if n == 0 {
		return
	}
	title := "Saved 1 file to the drive"
	if n > 1 {
		title = fmt.Sprintf("Saved %d files to the drive", n)
	}
	f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "finished", Title: title, Body: "Everything on this PC is on the drive."}})
	f.signal(agent.FSEvent{StatusChanged: true})
}

func mountID(seq uint64) string { return "t_" + strconv.FormatUint(seq, 10) }

func mountSeq(transferID string) (uint64, bool) {
	n, err := strconv.ParseUint(transferID, 10, 64)
	return n, err == nil
}

func errCode(err error) string {
	if e, ok := driveapi.AsError(err); ok && e.Code != "" {
		return e.Code
	}
	if driveapi.IsNetwork(err) {
		return "network"
	}
	return "error"
}

// mountTransfer builds the Transfer for a queued mount upload.
func (f *FS) mountTransfer(r *session, q vfs.Queued, drivePath string) agent.Transfer {
	if drivePath == "" && q.NodeID != "" {
		drivePath = r.vfs.PathOf(q.NodeID)
	}
	name := q.Name
	if drivePath != "" {
		name = path.Base(drivePath)
	}
	t := agent.Transfer{ID: mountID(q.Seq), Direction: "up", Origin: "mount", State: agent.TransferWaiting, Name: name,
		DrivePath: drivePath, LocalPath: f.localPath(drivePath), Size: q.Size, Attempts: q.Attempts, QueuedAt: q.QueuedAt.UTC()}
	if q.Active {
		t.State = agent.TransferActive
	}
	if p := r.prog.get(q.Seq); p != nil {
		t.BytesDone, t.BPS, t.Size = p.bytes, p.bps, max(t.Size, p.total)
		started := p.started
		t.StartedAt = &started
		if p.bps > 0 && t.Size > p.bytes {
			eta := (t.Size - p.bytes) / p.bps
			t.ETASeconds = &eta
		}
	}
	if q.LastError != "" && !q.Active {
		at := q.NotBefore.UTC()
		t.Error = &agent.TransferError{Code: q.LastError, Message: "Waiting to try again.", RetryAt: &at}
	}
	return t
}

func (f *FS) localPath(drivePath string) string {
	f.mu.Lock()
	dir := f.mountDir
	f.mu.Unlock()
	if dir == "" || drivePath == "" {
		return ""
	}
	return filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(drivePath, "/")))
}

// heldTransfer builds the Transfer of a held change, or nil.
func (f *FS) heldTransfer(r *session, seq uint64) *agent.Transfer {
	for _, h := range r.vfs.Held() {
		if h.Seq == seq {
			t := heldToTransfer(f, h)
			return &t
		}
	}
	return nil
}

func heldToTransfer(f *FS, h vfs.Held) agent.Transfer {
	actions := []string{"discard"}
	switch {
	case h.CanReupload:
		actions = []string{"reupload", "save_copy", "discard"}
	case h.Kind == journal.OpUpload && h.HasContent:
		actions = []string{"save_copy", "discard"}
	}
	return agent.Transfer{ID: mountID(h.Seq), Direction: "up", Origin: "mount", State: agent.TransferHeld, Name: path.Base(h.Path),
		DrivePath: h.Path, LocalPath: f.localPath(h.Path), Size: h.Size, QueuedAt: h.Since.UTC(),
		Held: &agent.Held{Code: h.Code, Message: h.Reason, Since: h.Since.UTC(), Actions: actions}}
}

// Status is the core's current condition.
func (f *FS) Status() agent.FSStatus {
	r := f.current()
	if r == nil {
		return agent.FSStatus{Connectivity: agent.Online, Summary: agent.Summary{}}
	}
	sum, unsent := f.summary(r)
	st := agent.FSStatus{Connectivity: agent.Online, Summary: sum, Unsent: unsent}
	if r.vfs.State() == vfs.StateOffline {
		st.Connectivity = agent.Offline
	}
	cs := r.cache.Stats()
	st.CacheFull = cs.Cap > 0 && cs.Unsent >= cs.Cap
	return st
}

// summaryCache keeps the summary for a moment: the agent asks on every
// transfer event, and computing it walks the queue.
type summaryCache struct {
	at     time.Time
	val    agent.Summary
	unsent agent.Unsent
}

func (f *FS) summary(r *session) (agent.Summary, agent.Unsent) {
	f.mu.Lock()
	if time.Since(f.sum.at) < 200*time.Millisecond {
		v, u := f.sum.val, f.sum.unsent
		f.mu.Unlock()
		return v, u
	}
	f.mu.Unlock()
	var s agent.Summary
	var u agent.Unsent
	for _, q := range r.vfs.Queued() {
		if q.Kind != journal.OpUpload {
			continue
		}
		u.Files++
		u.Bytes += q.Size
		if q.Active {
			s.Active++
		} else {
			s.Waiting++
		}
		s.BytesTotal += q.Size
	}
	for _, t := range r.sends.live() {
		if t.State == agent.TransferActive {
			s.Active++
		} else {
			s.Waiting++
		}
		s.BytesTotal += t.Size
		s.BytesDone += t.BytesDone // sends' progress; mount progress is added below
	}
	held := r.vfs.Held()
	s.Held, u.Held = len(held)+len(r.sends.heldList()), len(held)
	for _, h := range held {
		u.Bytes += h.Size
	}
	done, bps := r.prog.totals()
	s.BytesDone, s.UpBPS = min(s.BytesDone+done, s.BytesTotal), bps
	for _, b := range r.sends.list() {
		if b.FinishedAt == nil {
			s.UpBPS += b.BPS
		}
	}
	s.FilesLeft = s.Active + s.Waiting
	if bps > 0 && s.BytesTotal > s.BytesDone {
		eta := (s.BytesTotal - s.BytesDone) / bps
		s.ETASeconds = &eta
	}
	f.mu.Lock()
	f.sum = summaryCache{at: time.Now(), val: s, unsent: u}
	f.mu.Unlock()
	return s, u
}

// Transfers lists transfers by state, newest finished first.
func (f *FS) Transfers(ctx context.Context, q agent.TransferQuery) (agent.TransferPage, error) {
	r := f.current()
	want := func(state string) bool { return len(q.States) == 0 || slices.Contains(q.States, state) }
	var all []agent.Transfer
	var counts agent.TransferCounts
	if r != nil {
		for _, it := range r.vfs.Queued() {
			if it.Kind != journal.OpUpload {
				continue
			}
			state := agent.TransferWaiting
			if it.Active {
				state = agent.TransferActive
				counts.Active++
			} else {
				counts.Waiting++
			}
			if want(state) {
				all = append(all, f.mountTransfer(r, it, ""))
			}
		}
		for _, t := range r.sends.live() {
			if t.State == agent.TransferActive {
				counts.Active++
			} else {
				counts.Waiting++
			}
			if want(t.State) {
				all = append(all, t)
			}
		}
		held := r.vfs.Held()
		sendHeld := r.sends.heldList()
		counts.Held = len(held) + len(sendHeld)
		if want(agent.TransferHeld) {
			var hs []agent.Transfer
			for _, h := range held {
				hs = append(hs, heldToTransfer(f, h))
			}
			hs = append(hs, sendHeld...)
			slices.SortStableFunc(hs, func(a, b agent.Transfer) int { return b.Held.Since.Compare(a.Held.Since) })
			all = append(all, hs...)
		}
	}
	finished := f.hist.list()
	for _, t := range finished {
		switch t.State {
		case agent.TransferDone:
			counts.Done++
		case agent.TransferSkipped:
			counts.Skipped++
		case agent.TransferFailed:
			counts.Failed++
		}
		if want(t.State) {
			all = append(all, t)
		}
	}
	return page(all, q, counts)
}

// page cuts one page out of items; the cursor is an offset.
func page(items []agent.Transfer, q agent.TransferQuery, counts agent.TransferCounts) (agent.TransferPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	start := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(q.Cursor, "c_"))
		if err != nil || n < 0 {
			return agent.TransferPage{}, fmt.Errorf("%w: bad cursor", agent.ErrNotFound)
		}
		start = min(n, len(items))
	}
	end := min(start+limit, len(items))
	out := agent.TransferPage{Items: slices.Clone(items[start:end]), Counts: counts}
	if out.Items == nil {
		out.Items = []agent.Transfer{}
	}
	if end < len(items) {
		c := "c_" + strconv.Itoa(end)
		out.NextCursor = &c
	}
	return out, nil
}

// Retry is for failed sends; mount uploads retry on their own.
func (f *FS) Retry(ctx context.Context, id string) (agent.Transfer, error) {
	t, ok := f.hist.get(id)
	if !ok {
		return agent.Transfer{}, agent.ErrNotFound
	}
	r := f.current()
	if r == nil || t.Origin != "send" || t.State != agent.TransferFailed || t.LocalPath == "" {
		return agent.Transfer{}, agent.ErrNotRetryable
	}
	return r.sends.retry(ctx, t)
}

// Cancel is for sends; a mount upload is a saved file and is never cancelled.
func (f *FS) Cancel(ctx context.Context, id string) (agent.Transfer, error) {
	if strings.HasPrefix(id, "t_") {
		return agent.Transfer{}, agent.ErrNotCancellable
	}
	if r := f.current(); r != nil {
		if t, ok := r.sends.cancel(id); ok {
			return t, nil
		}
	}
	if _, ok := f.hist.get(id); ok {
		return agent.Transfer{}, agent.ErrNotCancellable
	}
	return agent.Transfer{}, agent.ErrNotFound
}

func (f *FS) heldSeq(id string) (*session, uint64, error) {
	r := f.current()
	if r == nil {
		return nil, 0, agent.ErrNotFound
	}
	seq, err := strconv.ParseUint(strings.TrimPrefix(id, "t_"), 10, 64)
	if err != nil || !strings.HasPrefix(id, "t_") {
		return nil, 0, agent.ErrNotFound
	}
	for _, h := range r.vfs.Held() {
		if h.Seq == seq {
			return r, seq, nil
		}
	}
	return nil, 0, agent.ErrNotHeld
}

// DiscardHeld drops a held change; the drive's state shows again.
func (f *FS) DiscardHeld(ctx context.Context, id string) error {
	if strings.HasPrefix(id, "b_") {
		r := f.current()
		if r == nil {
			return agent.ErrNotFound
		}
		t, ok := r.sends.discard(id)
		if !ok {
			return agent.ErrNotHeld
		}
		f.signal(agent.FSEvent{Held: &agent.HeldChange{Op: "removed", Item: t}})
		f.invalidate()
		return nil
	}
	r, seq, err := f.heldSeq(id)
	if err != nil {
		return err
	}
	if err := r.vfs.Discard(ctx, seq); err != nil {
		return fmt.Errorf("discard held change: %w", err)
	}
	f.invalidate()
	return nil
}

// ReuploadHeld queues a held file again: one the drive received with other
// bytes than this PC sent (checksum mismatch). Other refusals would only be
// refused again: they give agent.ErrNotRetryable.
func (f *FS) ReuploadHeld(ctx context.Context, id string) error {
	r, seq, err := f.heldSeq(id)
	if err != nil {
		return err
	}
	can := false
	for _, h := range r.vfs.Held() {
		if h.Seq == seq {
			can = h.CanReupload
		}
	}
	if !can {
		return agent.ErrNotRetryable
	}
	if err := r.vfs.Reupload(ctx, seq); err != nil {
		return fmt.Errorf("re-upload held change: %w", err)
	}
	f.invalidate()
	return nil
}

// SaveHeldCopy writes a held file's content into destDir and drops the
// held change.
func (f *FS) SaveHeldCopy(ctx context.Context, id, destDir string) (string, error) {
	r, seq, err := f.heldSeq(id)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(destDir); err != nil || !st.IsDir() {
		return "", agent.ErrLocalNotFound
	}
	p, err := r.vfs.SaveCopy(ctx, seq, destDir)
	if err != nil {
		return "", fmt.Errorf("save a copy: %w", err)
	}
	if err := r.vfs.Discard(ctx, seq); err != nil {
		f.log.Warn("core: held change kept after saving a copy", "err", err)
	}
	f.invalidate()
	return p, nil
}

func (f *FS) invalidate() {
	f.mu.Lock()
	f.sum = summaryCache{}
	f.mu.Unlock()
	f.signal(agent.FSEvent{StatusChanged: true})
}

// Pause holds uploads and downloads at their next part.
func (f *FS) Pause(ctx context.Context) error {
	f.mu.Lock()
	f.paused = true
	r := f.run
	f.mu.Unlock()
	if r != nil {
		r.eng.Pause()
	}
	return nil
}

// Resume lets transfers go again.
func (f *FS) Resume(ctx context.Context) error {
	f.mu.Lock()
	f.paused = false
	r := f.run
	f.mu.Unlock()
	if r != nil {
		r.eng.Resume()
	}
	return nil
}

// SetBandwidth applies a limit at once.
func (f *FS) SetBandwidth(b settings.Bandwidth) {
	f.mu.Lock()
	f.bw = b
	r := f.run
	f.mu.Unlock()
	if r != nil {
		r.eng.SetBandwidth(b.UpBPS, b.DownBPS)
	}
}

// releasePolicy turns the setting into the cache's timings.
func releasePolicy(s settings.CacheRelease) cache.ReleasePolicy {
	d := func(sec int64, def time.Duration) time.Duration {
		switch {
		case sec < 0:
			return def
		case sec == 0:
			return time.Nanosecond // at once
		}
		return time.Duration(sec) * time.Second
	}
	return cache.ReleasePolicy{AfterUpload: d(s.AfterUploadSeconds, cache.DefaultAfterUpload),
		KeepOpened: d(s.KeepOpenedSeconds, cache.DefaultKeepOpened)}
}

// SetCacheRelease applies new release timings at once.
func (f *FS) SetCacheRelease(s settings.CacheRelease) {
	if r := f.current(); r != nil {
		r.vfs.SetReleasePolicy(releasePolicy(s))
		f.invalidate()
	}
}

// SetCacheCap applies a new cache size cap at once (evicting down to it).
func (f *FS) SetCacheCap(n int64) {
	if r := f.current(); r != nil {
		r.cache.SetCap(n)
		f.invalidate()
	}
}

// Unsent counts changes not yet on the drive. While stopped it reads the
// last session's journal.
func (f *FS) Unsent(ctx context.Context) (agent.Unsent, error) {
	if r := f.current(); r != nil {
		var u agent.Unsent
		for _, q := range r.vfs.Queued() {
			if q.Kind == journal.OpUpload {
				u.Files++
				u.Bytes += q.Size
			}
		}
		for _, h := range r.vfs.Held() {
			u.Held++
			u.Bytes += h.Size
		}
		return u, nil
	}
	f.mu.Lock()
	dirs := f.last
	f.mu.Unlock()
	if dirs.state == "" {
		return agent.Unsent{}, nil
	}
	file := filepath.Join(dirs.state, "journal.log")
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return agent.Unsent{}, nil
	}
	jr, err := journal.Open(file)
	if err != nil {
		return agent.Unsent{}, fmt.Errorf("read journal: %w", err)
	}
	defer jr.Close()
	ops, _ := jr.List()
	var u agent.Unsent
	for _, op := range ops {
		switch {
		case op.State == journal.StateHeld:
			u.Held++
			u.Bytes += op.Size
		case op.Kind == journal.OpUpload:
			u.Files++
			u.Bytes += op.Size
		}
	}
	return u, nil
}

// ClearCache deletes every account's cached content, index and journal
// (unsent data included). Only the folders this program made are removed.
func (f *FS) ClearCache(ctx context.Context, cacheDir string) error {
	if f.current() != nil {
		return errors.New("core: stop the session before clearing the cache")
	}
	var errs []error
	for _, dir := range []string{cacheDir, filepath.Join(f.opts.StateDir, "accounts")} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "acct-") {
				if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	f.hist.clear()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("core: clear cache: %w", err)
	}
	f.log.Info("core: cache cleared")
	return nil
}

// WebURL is the drive web app's address for a drive path.
func (f *FS) WebURL(ctx context.Context, drivePath string) (string, error) {
	r := f.current()
	if r == nil {
		return "", errors.New("core: not signed in")
	}
	base := strings.TrimRight(r.webURL, "/")
	if drivePath == "" || drivePath == "/" {
		return base, nil
	}
	n, err := r.api.ResolvePath(ctx, drivePath)
	if driveapi.IsNotFound(err) {
		return "", agent.ErrDriveFolderNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find %s on the drive: %w", drivePath, err)
	}
	if n.URI != "" {
		return n.URI, nil
	}
	return base + "/n/" + n.ID, nil
}

// OpenFiles counts files open on the drive.
func (f *FS) OpenFiles() int {
	if r := f.current(); r != nil {
		return r.vfs.OpenFiles()
	}
	return 0
}
