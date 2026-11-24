package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/localfs"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// Sends (POST /v1/send, olsyn-drive send) run through transfer.Sender on
// the session's engine, the same one the mounted drive uploads through:
// pause, bandwidth, progress and the held list are shared. A batch is
// written to <account state>/sends/<id>.json when it starts and ends; an
// unfinished one runs again at the next start (the drive skips what it
// already has and resumes partial uploads).

const sendKeep = 7 * 24 * time.Hour

type sends struct {
	f   *FS
	r   *session
	dir string

	mu       sync.Mutex
	seq      int
	batches  map[string]*batchRun
	files    map[string]*sendFile // waiting and active, by transfer id
	held     []agent.Transfer     // oldest first
	stopping bool                 // the session is stopping: runs end unfinished
	wg       sync.WaitGroup
}

type batchRun struct {
	b       agent.Batch
	spec    savedSend
	cancel  context.CancelFunc
	doneB   int64            // bytes of files done
	live    map[string]int64 // bytes sent of active files
	sampleT time.Time
	sampleB int64
	running bool
}

type sendFile struct {
	t         agent.Transfer
	batch     *batchRun
	cancel    context.CancelFunc
	cancelled bool
	at        time.Time // last progress sample
}

// savedSend is a batch on disk.
type savedSend struct {
	ParentID          string      `json:"parent_id"`
	ParentPath        string      `json:"parent_path"`
	Sources           []string    `json:"sources"`
	Conflict          string      `json:"conflict"`
	IncludeOnlineOnly bool        `json:"include_online_only"`
	Batch             agent.Batch `json:"batch"`
}

func newSends(f *FS, r *session) *sends {
	return &sends{f: f, r: r, dir: filepath.Join(r.dirs.state, "sends"), batches: map[string]*batchRun{}, files: map[string]*sendFile{}}
}

// load reads saved batches and runs the unfinished ones again.
func (s *sends) load(ctx context.Context) {
	entries, _ := os.ReadDir(s.dir)
	cut := time.Now().Add(-sendKeep)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(p)
		var sv savedSend
		if err != nil || json.Unmarshal(data, &sv) != nil || sv.Batch.ID == "" {
			continue
		}
		if sv.Batch.FinishedAt != nil && sv.Batch.FinishedAt.Before(cut) {
			_ = os.Remove(p)
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(sv.Batch.ID, "b_")); err == nil && n > s.seq {
			s.seq = n
		}
		br := &batchRun{b: sv.Batch, spec: sv}
		s.batches[sv.Batch.ID] = br
		if sv.Batch.FinishedAt == nil {
			// counted again from the start: what reached the drive before
			// shows as skipped (already there)
			b := &br.b
			b.State, b.Files, b.FilesDone, b.FilesSkipped, b.FilesFailed, b.FilesHeld, b.NotLocal = agent.BatchScanning, 0, 0, 0, 0, 0, 0
			b.Bytes, b.BytesDone, b.BPS = 0, 0, 0
			s.f.log.Info("core: carrying on with an unfinished send", "batch", sv.Batch.ID)
			s.start(ctx, br, sv.Sources, sv.ParentID, sv.ParentPath, sv.Batch.ID)
		}
	}
}

func (s *sends) save(br *batchRun) {
	s.mu.Lock()
	sv := br.spec
	sv.Batch = br.b
	s.mu.Unlock()
	data, err := json.Marshal(sv)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.f.log.Warn("core: save send", "err", err)
		return
	}
	tmp := filepath.Join(s.dir, sv.Batch.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, filepath.Join(s.dir, sv.Batch.ID+".json"))
	}
}

// Send starts a batch.
func (f *FS) Send(ctx context.Context, req agent.SendRequest) (agent.SendResult, error) {
	r := f.current()
	if r == nil || r.sends == nil {
		return agent.SendResult{}, errors.New("core: not signed in")
	}
	parent, parentPath, err := f.destination(ctx, r, req.DriveFolder, req.DriveFolderID)
	if err != nil {
		return agent.SendResult{}, err
	}
	if ok, why := writable(parent); !ok {
		return agent.SendResult{}, fmt.Errorf("%w (%s)", agent.ErrDriveFolderReadOnly, why)
	}
	conflict := req.OnConflict
	switch conflict {
	case "", transfer.OnConflictHold, transfer.OnConflictNewVersion, transfer.OnConflictKeepBoth:
	default:
		return agent.SendResult{}, fmt.Errorf("core: unknown conflict policy %q", conflict)
	}
	files, bytes := countLocal(ctx, req.LocalPaths, req.IncludeOnlineOnly)
	s := r.sends
	s.mu.Lock()
	s.seq++
	id := "b_" + strconv.Itoa(s.seq)
	br := &batchRun{spec: savedSend{ParentID: parent.ID, ParentPath: parentPath, Sources: slices.Clone(req.LocalPaths),
		Conflict: conflict, IncludeOnlineOnly: req.IncludeOnlineOnly},
		b: agent.Batch{ID: id, State: agent.BatchScanning, DriveFolder: parentPath, LocalPaths: slices.Clone(req.LocalPaths),
			StartedAt: time.Now().UTC()}}
	s.batches[id] = br
	s.mu.Unlock()
	f.log.Info("core: send started", "batch", id, "items", len(req.LocalPaths), "files", files, "bytes", bytes)
	s.save(br)
	s.start(r.ctx, br, req.LocalPaths, parent.ID, parentPath, id)
	return agent.SendResult{BatchID: id, Files: files, Bytes: bytes}, nil
}

// destination finds the drive folder of a send.
// A drive that is restarting or unreachable is retried for a while (the
// person may be just back online); after that the send is refused with
// agent.ErrDriveUnreachable.
func (f *FS) destination(ctx context.Context, r *session, drivePath, id string) (*driveapi.Node, string, error) {
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var n *driveapi.Node
	var p string
	err := driveapi.Retry(wctx, f.opts.Backoff, 0, func(ctx context.Context) error {
		var err error
		n, p, err = f.lookupDestination(ctx, r, drivePath, id)
		return err
	})
	if err != nil && (driveapi.IsRetryable(err) || driveapi.IsNetwork(err)) {
		return nil, "", fmt.Errorf("%w: %v", agent.ErrDriveUnreachable, err)
	}
	if err != nil {
		return nil, "", mapDriveErr(err)
	}
	return n, p, nil
}

func (f *FS) lookupDestination(ctx context.Context, r *session, drivePath, id string) (*driveapi.Node, string, error) {
	if id != "" {
		n, err := r.api.GetNode(ctx, id)
		if driveapi.IsNotFound(err) {
			return nil, "", agent.ErrDriveFolderNotFound
		}
		if err != nil {
			return nil, "", fmt.Errorf("find the drive folder: %w", err)
		}
		p := r.vfs.PathOf(id)
		if p == "" {
			p = crumbsPath(n)
		}
		if !n.Node.IsDir() {
			return nil, "", agent.ErrDriveFolderNotFound
		}
		return &n.Node, p, nil
	}
	p := "/" + strings.Trim(path.Clean("/"+drivePath), "/")
	n, err := r.api.ResolvePath(ctx, p)
	if driveapi.IsNotFound(err) {
		return nil, "", agent.ErrDriveFolderNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("find %s on the drive: %w", p, err)
	}
	if !n.IsDir() {
		return nil, "", agent.ErrDriveFolderNotFound
	}
	return n, p, nil
}

// countLocal counts what a send will read, for the answer to POST /v1/send.
// It gives up after a few seconds on a huge tree (the batch events carry
// the real totals).
func countLocal(ctx context.Context, paths []string, online bool) (files int, bytes int64) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, p := range paths {
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			files++
			bytes += st.Size()
			continue
		}
		_ = localfs.Walk(ctx, p, localfs.WalkOptions{IncludeOnlineOnly: online}, func(e localfs.Entry) error {
			files++
			bytes += e.Size
			return nil
		}, func(localfs.Skip) {})
	}
	return files, bytes
}

// start runs a batch (or part of one: a retried file) in the background.
func (s *sends) start(ctx context.Context, br *batchRun, sources []string, parentID, parentPath, runID string) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	br.cancel, br.running = cancel, true
	if br.live == nil {
		br.live = map[string]int64{}
	}
	if br.b.State != agent.BatchScanning {
		br.b.State = agent.BatchSending
	}
	br.b.FinishedAt, br.b.Error = nil, nil
	s.mu.Unlock()
	spec := transfer.SendSpec{BatchID: runID, Sources: sources, ParentID: parentID, ParentPath: parentPath,
		Conflict: br.spec.Conflict, IncludeOnlineOnly: br.spec.IncludeOnlineOnly}
	sender := &transfer.Sender{API: s.r.api, Engine: s.r.eng, Backoff: s.f.opts.Backoff, Logger: s.f.log}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		err := sender.Send(ctx, spec, s.report(br))
		s.finish(br, ctx, err)
	}()
}

func (s *sends) report(br *batchRun) transfer.SendReport {
	return transfer.SendReport{
		Found: func(sf transfer.SendFile) {
			t := agent.Transfer{ID: sf.ID, Direction: "up", Origin: "send", State: agent.TransferWaiting,
				Name: path.Base(sf.DrivePath), LocalPath: sf.Local, DrivePath: sf.DrivePath, Size: sf.Size,
				QueuedAt: time.Now().UTC(), BatchID: &br.b.ID}
			s.mu.Lock()
			br.b.Files++
			br.b.Bytes += sf.Size
			if sf.Local != "" {
				s.files[sf.ID] = &sendFile{t: t, batch: br}
			}
			s.mu.Unlock()
			if sf.Local != "" {
				s.f.signal(agent.FSEvent{Transfer: &t})
			}
		},
		Start: func(ctx context.Context, sf transfer.SendFile) context.Context {
			ctx, cancel := context.WithCancel(ctx)
			s.mu.Lock()
			defer s.mu.Unlock()
			it := s.files[sf.ID]
			if it == nil {
				cancel()
				return nil
			}
			it.cancel = cancel
			if it.cancelled {
				cancel()
			}
			now := time.Now().UTC()
			it.t.State, it.t.StartedAt, it.t.Attempts, it.t.Error = agent.TransferActive, &now, it.t.Attempts+1, nil
			return ctx
		},
		Waiting: func(sf transfer.SendFile, err error, at time.Time) {
			s.mu.Lock()
			it := s.files[sf.ID]
			var t agent.Transfer
			if it != nil {
				retry := at.UTC()
				it.t.State = agent.TransferWaiting
				it.t.Error = &agent.TransferError{Code: errCode(err), Message: "Waiting to try again: " + driveapi.Reason(err), RetryAt: &retry}
				t = it.t
			}
			s.mu.Unlock()
			if it != nil {
				s.f.signal(agent.FSEvent{Transfer: &t})
			}
		},
		Result: func(sf transfer.SendFile, outcome, code, reason string) { s.result(br, sf, outcome, code, reason) },
		Scanned: func() {
			s.mu.Lock()
			if br.b.State == agent.BatchScanning {
				br.b.State = agent.BatchSending
			}
			b := br.b
			s.mu.Unlock()
			s.f.signal(agent.FSEvent{Batch: &b})
		},
	}
}

func (s *sends) result(br *batchRun, sf transfer.SendFile, outcome, code, reason string) {
	now := time.Now().UTC()
	s.mu.Lock()
	it := s.files[sf.ID]
	delete(s.files, sf.ID)
	delete(br.live, sf.ID)
	t := agent.Transfer{ID: sf.ID, Direction: "up", Origin: "send", Name: path.Base(sf.DrivePath), LocalPath: sf.Local,
		DrivePath: sf.DrivePath, Size: sf.Size, QueuedAt: now, BatchID: &br.b.ID}
	if it != nil {
		t = it.t
	}
	t.FinishedAt, t.BPS, t.ETASeconds = &now, 0, nil
	var held *agent.HeldChange
	switch outcome {
	case transfer.SendDone:
		t.State, t.BytesDone, t.Error = agent.TransferDone, sf.Size, nil
		br.b.FilesDone++
		br.doneB += sf.Size
	case transfer.SendSkipped:
		t.State, t.SkippedReason, t.Error = agent.TransferSkipped, &code, nil
		if code == "already_there" {
			br.doneB += sf.Size
		}
		br.b.FilesSkipped++
	case transfer.SendNotLocal:
		nd := "not_downloaded"
		t.State, t.SkippedReason = agent.TransferSkipped, &nd
		br.b.NotLocal++
	case transfer.SendHeld:
		t.State, t.FinishedAt = agent.TransferHeld, nil
		t.Held = &agent.Held{Code: code, Message: reason, Since: now, Actions: []string{"discard"}}
		br.b.FilesHeld++
		s.held = append(s.held, t)
		held = &agent.HeldChange{Op: "added", Item: t}
	case transfer.SendCancelled:
		if code == "cancelled" && it != nil && !it.cancelled {
			// the session stopped: the batch carries on at the next start
			s.mu.Unlock()
			return
		}
		t.State = agent.TransferCancelled
		t.Error = &agent.TransferError{Code: code, Message: reason}
	default:
		t.State = agent.TransferFailed
		t.Error = &agent.TransferError{Code: code, Message: reason}
		br.b.FilesFailed++
	}
	t.Can = agent.TransferCan{Retry: t.State == agent.TransferFailed && t.LocalPath != ""}
	b := s.batchLocked(br)
	s.mu.Unlock()
	if t.State != agent.TransferHeld {
		s.f.hist.add(t)
	}
	s.f.signal(agent.FSEvent{Transfer: &t})
	if held != nil {
		s.f.signal(agent.FSEvent{Held: held})
	}
	s.f.signal(agent.FSEvent{Batch: &b})
}

// batchLocked fills in the batch's progress figures.
func (s *sends) batchLocked(br *batchRun) agent.Batch {
	var live int64
	for _, n := range br.live {
		live += n
	}
	br.b.BytesDone = min(br.doneB+live, br.b.Bytes)
	now := time.Now()
	if dt := now.Sub(br.sampleT).Seconds(); dt >= 1 {
		if !br.sampleT.IsZero() {
			rate := int64(float64(br.b.BytesDone-br.sampleB) / dt)
			br.b.BPS = max(0, (br.b.BPS+rate)/2)
		}
		br.sampleT, br.sampleB = now, br.b.BytesDone
	}
	br.b.ETASeconds = nil
	if br.b.BPS > 0 && br.b.Bytes > br.b.BytesDone {
		eta := (br.b.Bytes - br.b.BytesDone) / br.b.BPS
		br.b.ETASeconds = &eta
	}
	return br.b
}

// finish closes a batch run.
func (s *sends) finish(br *batchRun, ctx context.Context, err error) {
	s.mu.Lock()
	br.running = false
	if s.stopping && !errors.Is(err, transfer.ErrSendStopped) {
		s.mu.Unlock()
		s.save(br) // unfinished: runs again at the next start
		return
	}
	now := time.Now().UTC()
	br.b.FinishedAt = &now
	br.b.BPS, br.b.ETASeconds = 0, nil
	switch {
	case err != nil && !errors.Is(err, context.Canceled):
		br.b.State = agent.BatchFailed
		msg := driveapi.Reason(err)
		if errors.Is(err, transfer.ErrSendStopped) {
			msg = strings.TrimPrefix(err.Error(), transfer.ErrSendStopped.Error()+": ")
		}
		br.b.Error = &agent.Problem{Code: "send_stopped", Message: msg}
	case err != nil:
		br.b.State = agent.BatchCancelled
	default:
		br.b.State = agent.BatchDone
	}
	b := s.batchLocked(br)
	s.mu.Unlock()
	s.save(br)
	s.f.log.Info("core: send finished", "batch", b.ID, "state", b.State, "done", b.FilesDone, "skipped", b.FilesSkipped,
		"held", b.FilesHeld, "failed", b.FilesFailed, "not_local", b.NotLocal)
	s.f.signal(agent.FSEvent{Batch: &b})
	s.notifyFinished(b)
	s.f.invalidate()
}

func (s *sends) notifyFinished(b agent.Batch) {
	where := b.DriveFolder
	if where == "/" {
		where = "the top of the drive"
	}
	if b.State == agent.BatchFailed && b.Error != nil {
		s.f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "error", Title: "Sending to " + where + " stopped",
			Body: b.Error.Message}})
		return
	}
	left := b.FilesHeld + b.FilesFailed + b.NotLocal
	if left > 0 {
		s.f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "held", Title: plural(left, "file") + " not sent to " + where,
			Body:   "The others are on the drive. The list says why for each one.",
			Action: &agent.NotificationAction{Label: "Show", Open: agent.OpenAction{Target: "ui", Path: "transfers/held"}}}})
		return
	}
	s.f.signal(agent.FSEvent{Notification: &agent.Notification{Kind: "finished", Title: "Sent " + plural(b.FilesDone+b.FilesSkipped, "file") + " to " + where,
		Body: "Everything is on the drive."}})
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// progress takes an engine event for a send file.
func (s *sends) progress(e *transfer.Event) {
	s.mu.Lock()
	it := s.files[e.TransferID]
	if it == nil {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	if d := e.Bytes - it.t.BytesDone; d > 0 && !it.at.IsZero() {
		if dt := now.Sub(it.at).Seconds(); dt > 0.05 {
			it.t.BPS = (it.t.BPS + int64(float64(d)/dt)) / 2
		}
	}
	it.at = now
	if e.Kind == transfer.EventStarted || e.Kind == transfer.EventProgress {
		it.t.State = agent.TransferActive
	}
	it.t.BytesDone = max(e.Bytes, 0)
	it.t.ETASeconds = nil
	if it.t.BPS > 0 && it.t.Size > it.t.BytesDone {
		eta := (it.t.Size - it.t.BytesDone) / it.t.BPS
		it.t.ETASeconds = &eta
	}
	it.batch.live[e.TransferID] = it.t.BytesDone
	t := it.t
	b := s.batchLocked(it.batch)
	s.mu.Unlock()
	s.f.signal(agent.FSEvent{Transfer: &t})
	s.f.signal(agent.FSEvent{Batch: &b})
}

// live returns the waiting and active send transfers: active first.
func (s *sends) live() []agent.Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Transfer, 0, len(s.files))
	for _, it := range s.files {
		t := it.t
		t.Can = agent.TransferCan{Cancel: true}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b agent.Transfer) int {
		if (a.State == agent.TransferActive) != (b.State == agent.TransferActive) {
			if a.State == agent.TransferActive {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (s *sends) heldList() []agent.Transfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.held)
}

func (s *sends) list() []agent.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Batch, 0, len(s.batches))
	for _, br := range s.batches {
		out = append(out, s.batchLocked(br))
	}
	slices.SortFunc(out, func(a, b agent.Batch) int { return b.StartedAt.Compare(a.StartedAt) })
	return out
}

// cancel stops one waiting or active send file.
func (s *sends) cancel(id string) (agent.Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.files[id]
	if it == nil {
		return agent.Transfer{}, false
	}
	it.cancelled = true
	if it.cancel != nil {
		it.cancel()
	}
	t := it.t
	t.State = agent.TransferCancelled
	return t, true
}

// discard drops a held send file from the list (the file on this PC stays).
func (s *sends) discard(id string) (agent.Transfer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.held {
		if t.ID == id {
			s.held = slices.Delete(s.held, i, i+1)
			return t, true
		}
	}
	return agent.Transfer{}, false
}

// retry sends a failed file again, in its batch.
func (s *sends) retry(ctx context.Context, t agent.Transfer) (agent.Transfer, error) {
	s.mu.Lock()
	if t.BatchID == nil {
		s.mu.Unlock()
		return agent.Transfer{}, agent.ErrNotRetryable
	}
	br := s.batches[*t.BatchID]
	if br == nil {
		s.mu.Unlock()
		return agent.Transfer{}, agent.ErrNotRetryable
	}
	br.b.FilesFailed = max(0, br.b.FilesFailed-1)
	br.b.Files--
	br.b.Bytes -= t.Size
	s.seq++
	runID := *t.BatchID + "r" + strconv.Itoa(s.seq)
	s.mu.Unlock()
	dir := path.Dir(t.DrivePath)
	parent, err := s.r.api.ResolvePath(ctx, dir)
	if err != nil {
		return agent.Transfer{}, fmt.Errorf("find %s on the drive: %w", dir, mapDriveErr(err))
	}
	s.f.hist.remove(t.ID)
	s.start(s.r.ctx, br, []string{t.LocalPath}, parent.ID, dir, runID)
	t.State, t.Error, t.FinishedAt = agent.TransferWaiting, nil, nil
	return t, nil
}

// stop cancels every run and waits for them.
func (s *sends) stop() {
	s.mu.Lock()
	s.stopping = true
	for _, br := range s.batches {
		if br.cancel != nil {
			br.cancel()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}
