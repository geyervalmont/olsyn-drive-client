// Package spool runs drop folders: people (or their own copy tools) put
// files into <root>/<person folder>/<label>/ (shared node) or
// <root>/<label>/ (personal node); when a file has stopped changing it is
// sent to the drop's drive folder, verified against the drive's own hash,
// and only then deleted. Anything the drive refuses stays where it is.
//
// The spool is untrusted input: links are never followed, names are
// checked, and a delete re-checks that the file is still the one that was
// verified and still inside the spool.
package spool

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/engine"
	"olsyn-drive/internal/fsx"
	"olsyn-drive/internal/state"
)

// FullMarker is dropped into every drop folder while the spool is full.
const FullMarker = "SPOOL FULL - try again later.txt"

// Config shapes a spool.
type Config struct {
	Root       string
	Poll       time.Duration // how often folders are scanned (default 5 s)
	Settle     time.Duration // unchanged this long = complete (default 20 s)
	ZeroSettle time.Duration // an empty file waits longer (placeholders) (default 120 s)
	Refresh    time.Duration // how often drops are fetched (default 60 s)
	MaxBytes   int64         // the spool holds at most this much (0 = no cap)
	FreeFloor  int64         // keep at least this much free on the disk (default 2 GiB)
	Files      int           // files in flight per drop (default 4)
	RetryAfter time.Duration // a failed file is tried again after this (default 10 min)
	Version    string
	Machine    string
}

// Spool is a running spool.
type Spool struct {
	cfg   Config
	api   *driveapi.Client
	eng   *engine.Engine
	store *state.Store
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex
	self     *driveapi.Self
	drops    map[string]*dropRun // drop id → run
	seen     map[string]seen     // abs path → when its current size/time was first seen
	inflight map[string]bool     // abs paths handed to a run
	stats    Stats
}

type seen struct {
	size, mtime int64
	since       time.Time
}

type dropRun struct {
	drop driveapi.Drop
	dir  string
	run  *engine.Run
	stop context.CancelFunc
}

// Stats are the spool's numbers for status, metrics and the heartbeat.
type Stats struct {
	Waiting     int64     `json:"waiting"` // settling: still arriving or just arrived
	Queued      int64     `json:"queued"`
	Sending     int64     `json:"sending"`
	Held        int64     `json:"held"`
	Failed      int64     `json:"failed"`
	Unmapped    int64     `json:"unmapped"` // files in folders no drop matches
	SpoolBytes  int64     `json:"spool_bytes"`
	FreeBytes   int64     `json:"free_bytes"`
	Full        bool      `json:"full"`
	SentFiles   int64     `json:"sent_files"`
	SentBytes   int64     `json:"sent_bytes"`
	LastScan    time.Time `json:"last_scan"`
	LastError   string    `json:"last_error,omitempty"`
	Drops       int       `json:"drops"`
	HeldReasons []Problem `json:"held_reasons,omitempty"`
}

// Problem is a file needing a person, for the status page.
type Problem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// New makes a spool; Run starts it.
func New(cfg Config, api *driveapi.Client, store *state.Store, log *slog.Logger) *Spool {
	if cfg.Poll == 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.Settle == 0 {
		cfg.Settle = 20 * time.Second
	}
	if cfg.ZeroSettle == 0 {
		cfg.ZeroSettle = 120 * time.Second
	}
	if cfg.Refresh == 0 {
		cfg.Refresh = time.Minute
	}
	if cfg.FreeFloor == 0 {
		cfg.FreeFloor = 2 << 30
	}
	if cfg.RetryAfter == 0 {
		cfg.RetryAfter = 10 * time.Minute
	}
	s := &Spool{cfg: cfg, api: api, store: store, log: log, now: time.Now,
		drops: map[string]*dropRun{}, seen: map[string]seen{}, inflight: map[string]bool{}}
	s.eng = &engine.Engine{API: api, Store: store, Log: log}
	return s
}

// Engine exposes the engine (tests shorten its backoff).
func (s *Spool) Engine() *engine.Engine { return s.eng }

// Stats returns a copy of the current numbers.
func (s *Spool) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.HeldReasons = append([]Problem(nil), st.HeldReasons...)
	return st
}

// Run scans until ctx ends, then lets in-flight files stop at a safe
// point (their records say where).
func (s *Spool) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.Root, 0o755); err != nil {
		return err
	}
	var lastRefresh, lastBeat time.Time
	t := time.NewTicker(s.cfg.Poll)
	defer t.Stop()
	for {
		if s.now().Sub(lastRefresh) >= s.cfg.Refresh || s.self == nil {
			if err := s.refresh(ctx); err != nil {
				s.setErr(err)
				if driveapi.StatusOf(err) == 401 {
					s.stopAll()
					return fmt.Errorf("the drive no longer accepts this node's sign-in: %w", err)
				}
			} else {
				lastRefresh = s.now()
			}
		}
		s.Scan(ctx)
		if s.now().Sub(lastBeat) >= s.cfg.Refresh {
			s.heartbeat(ctx)
			lastBeat = s.now()
		}
		select {
		case <-ctx.Done():
			s.stopAll()
			return nil
		case <-t.C:
		}
	}
}

func (s *Spool) setErr(err error) {
	s.mu.Lock()
	s.stats.LastError = err.Error()
	s.mu.Unlock()
	s.log.Warn("spool", "err", err.Error())
}

func (s *Spool) stopAll() {
	s.mu.Lock()
	runs := make([]*dropRun, 0, len(s.drops))
	for _, d := range s.drops {
		runs = append(runs, d)
	}
	s.drops = map[string]*dropRun{}
	s.mu.Unlock()
	for _, d := range runs {
		d.stop()
		d.run.Wait()
	}
}

// refresh fetches this node's drops and makes their folders.
func (s *Spool) refresh(ctx context.Context) error {
	self, err := s.api.Self(ctx)
	if err != nil {
		return err
	}
	want := map[string]driveapi.Drop{}
	for _, d := range self.Drops {
		want[d.ID] = d
	}
	s.mu.Lock()
	s.self = self
	var gone []*dropRun
	for id, d := range s.drops {
		if _, ok := want[id]; !ok {
			gone = append(gone, d)
			delete(s.drops, id)
		}
	}
	s.mu.Unlock()
	for _, d := range gone {
		// A revoked drop: stop feeding it; files stay in place.
		s.log.Info("drop removed", "drop", d.drop.ID)
		d.stop()
		go d.run.Wait()
	}
	for id, d := range want {
		dir, err := s.dropDir(self, d)
		if err != nil {
			s.log.Warn("drop folder", "drop", id, "err", err.Error())
			continue
		}
		if err := os.MkdirAll(dir, 0o775); err != nil {
			s.log.Warn("drop folder", "drop", id, "err", err.Error())
			continue
		}
		s.mu.Lock()
		if _, ok := s.drops[id]; !ok {
			rctx, cancel := context.WithCancel(context.Background())
			dr := &dropRun{drop: d, dir: dir, stop: cancel}
			person, assurance := d.Person.ID, ""
			if self.Mode == "personal" {
				person = "" // a personal node speaks for its own person
			}
			dr.run = s.eng.Start(rctx, engine.RunOptions{Dest: d.FolderID, Person: person, Assurance: assurance,
				Conflict: d.OnConflict, Files: s.cfg.Files, Verify: true, KeyScope: "spool|" + d.ID,
				OnResult: func(it engine.Item, r engine.Result) { s.result(dr, it, r) }})
			s.drops[id] = dr
		}
		s.mu.Unlock()
	}
	return nil
}

// dropDir is where a drop lives in the spool: <person folder>/<label> on
// a shared node, <label> on a personal one. Names come from the drive,
// which sanitises them; they are checked again here.
func (s *Spool) dropDir(self *driveapi.Self, d driveapi.Drop) (string, error) {
	parts := []string{d.Label}
	if self.Mode != "personal" {
		parts = []string{d.PersonFolder, d.Label}
	}
	for _, p := range parts {
		if why := fsx.CheckName(p); why != "" || fsx.WindowsNameProblem(p) != "" {
			return "", fmt.Errorf("unusable folder name %q", p)
		}
	}
	return filepath.Join(append([]string{s.cfg.Root}, parts...)...), nil
}

// Scan walks every drop folder once: settles files, hands complete ones
// to their drop's run, and keeps the disk cap.
func (s *Spool) Scan(ctx context.Context) {
	s.mu.Lock()
	drops := make([]*dropRun, 0, len(s.drops))
	for _, d := range s.drops {
		drops = append(drops, d)
	}
	s.mu.Unlock()
	sort.Slice(drops, func(i, j int) bool { return drops[i].dir < drops[j].dir })
	var total, waiting, queued, held, failed int64
	var problems []Problem
	now := s.now()
	stillSeen := map[string]bool{}
	for _, d := range drops {
		skips, err := fsx.Walk(d.dir, fsx.WalkOptions{}, func(e fsx.Entry) error {
			total += e.Size
			stillSeen[e.Abs] = true
			s.mu.Lock()
			busy := s.inflight[e.Abs]
			s.mu.Unlock()
			if busy {
				queued++
				return nil
			}
			key := state.KeyFor("spool|"+d.drop.ID, e.Rel)
			if rec, _ := s.store.Get(key); rec != nil && rec.Size == e.Size && rec.MtimeNs == e.MtimeNs {
				switch {
				case rec.State == state.Held:
					held++
					problems = append(problems, Problem{d.display(e.Rel), rec.Reason})
					return nil
				case rec.State == state.Failed && now.Sub(rec.UpdatedAt) < s.cfg.RetryAfter:
					failed++
					problems = append(problems, Problem{d.display(e.Rel), rec.Reason})
					return nil
				}
			}
			if !s.settled(e, now) {
				waiting++
				return nil
			}
			if err := fsx.OpenExclusive(e.Abs); err != nil {
				waiting++ // still open for writing (Windows share modes)
				return nil
			}
			s.mu.Lock()
			s.inflight[e.Abs] = true
			s.mu.Unlock()
			queued++
			return d.run.Add(engine.Item{Abs: e.Abs, Rel: e.Rel, Size: e.Size, MtimeNs: e.MtimeNs})
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("scan", "dir", d.dir, "err", err.Error())
		}
		for _, sk := range skips {
			if strings.HasPrefix(sk.Reason, "a temporary file") {
				waiting++
				continue
			}
			held++
			problems = append(problems, Problem{d.display(sk.Rel), "not sent: " + sk.Reason})
		}
	}
	unmapped := s.unmapped(drops)
	s.mu.Lock()
	for p := range s.seen {
		if !stillSeen[p] {
			delete(s.seen, p) // gone (sent, moved away)
		}
	}
	free := freeBytes(s.cfg.Root)
	full := (s.cfg.MaxBytes > 0 && total >= s.cfg.MaxBytes) || (free >= 0 && free < s.cfg.FreeFloor)
	wasFull := s.stats.Full
	sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	if len(problems) > 200 {
		problems = problems[:200]
	}
	s.stats.Waiting, s.stats.Queued, s.stats.Held, s.stats.Failed = waiting, queued, held, failed
	s.stats.SpoolBytes, s.stats.FreeBytes, s.stats.Full, s.stats.LastScan = total, free, full, now
	s.stats.Unmapped, s.stats.HeldReasons, s.stats.Drops = unmapped, problems, len(drops)
	s.mu.Unlock()
	if full != wasFull {
		s.setFull(drops, full)
	}
}

func (d *dropRun) display(rel string) string {
	if d.drop.Path != "" {
		return strings.TrimRight(d.drop.Path, `\/`) + "/" + rel
	}
	return d.drop.Label + "/" + rel
}

// settled: the file's size and time have stayed put long enough.
func (s *Spool) settled(e fsx.Entry, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.seen[e.Abs]
	if !ok || prev.size != e.Size || prev.mtime != e.MtimeNs {
		s.seen[e.Abs] = seen{e.Size, e.MtimeNs, now}
		return false
	}
	wait := s.cfg.Settle
	if e.Size == 0 {
		wait = s.cfg.ZeroSettle
	}
	// A time in the future (another machine's clock over SMB) doesn't
	// matter: settling counts from when this node first saw it.
	return now.Sub(prev.since) >= wait
}

// unmapped counts files outside every drop folder (a revoked drop, a
// folder someone made by hand). They are left alone.
func (s *Spool) unmapped(drops []*dropRun) int64 {
	inDrop := map[string]bool{}
	for _, d := range drops {
		inDrop[d.dir] = true
	}
	var n int64
	fsx.Walk(s.cfg.Root, fsx.WalkOptions{}, func(e fsx.Entry) error {
		for dir := filepath.Dir(e.Abs); len(dir) >= len(s.cfg.Root); dir = filepath.Dir(dir) {
			if inDrop[dir] {
				return nil
			}
			if dir == s.cfg.Root {
				break
			}
		}
		if e.Rel != FullMarker {
			n++
		}
		return nil
	})
	return n
}

// result handles a finished file: verified ones are deleted, the rest
// stay where they are.
func (s *Spool) result(d *dropRun, it engine.Item, r engine.Result) {
	s.mu.Lock()
	delete(s.inflight, it.Abs)
	s.mu.Unlock()
	switch r.Outcome {
	case engine.Sent, engine.Already:
		if err := s.remove(d.dir, it); err != nil {
			s.log.Warn("kept after upload", "path", d.display(it.Rel), "why", err.Error())
			return
		}
		s.mu.Lock()
		if r.Outcome == engine.Sent {
			s.stats.SentFiles++
			s.stats.SentBytes += it.Size
		}
		s.mu.Unlock()
		s.log.Info("delivered", "path", d.display(it.Rel), "outcome", r.Outcome, "bytes", it.Size)
	case engine.Pending:
		// Sent; checked again on a later scan (it's re-added then).
	default:
		s.log.Info("held", "path", d.display(it.Rel), "outcome", r.Outcome, "why", r.Reason)
	}
}

// remove deletes a verified file, but only if it is still exactly what
// was verified (same size and time, one link, a regular file) and no
// folder between it and the drop folder has become a link. Then empty
// folders are tidied up to (not including) the drop folder.
func (s *Spool) remove(dropDir string, it engine.Item) error {
	p, err := fsx.Join(dropDir, it.Rel)
	if err != nil || p != it.Abs {
		return fmt.Errorf("path changed")
	}
	for dir := filepath.Dir(p); dir != dropDir; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() || st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("a folder on the way became something else")
		}
		if len(dir) <= len(dropDir) {
			return fmt.Errorf("outside the drop folder")
		}
	}
	st, err := os.Lstat(p)
	if err != nil {
		return err
	}
	a := fsx.AttrsOf(p, st)
	if !st.Mode().IsRegular() && !a.Cloud || st.Size() != it.Size || st.ModTime().UnixNano() != it.MtimeNs {
		return fmt.Errorf("it changed after it was verified; it will be sent again")
	}
	if a.Links > 1 {
		return fmt.Errorf("it has other hard links; left in place")
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	for dir := filepath.Dir(p); dir != dropDir && len(dir) > len(dropDir); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // fails while not empty: stop
			break
		}
	}
	return nil
}

// setFull puts or takes away the "full" marker in each drop folder and,
// on macOS/Linux, makes drop folders read-only so copies fail at once
// instead of filling the disk.
func (s *Spool) setFull(drops []*dropRun, full bool) {
	for _, d := range drops {
		marker := filepath.Join(d.dir, FullMarker)
		if full {
			os.WriteFile(marker, []byte("This drop folder is full. Files already here are being sent; try again later.\r\n"), 0o444)
		} else {
			os.Chmod(marker, 0o644)
			os.Remove(marker)
		}
		lockDir(d.dir, full)
	}
	s.log.Warn("spool full", "full", full)
}

func (s *Spool) heartbeat(ctx context.Context) {
	st := s.Stats()
	free, spool := st.FreeBytes, st.SpoolBytes
	hb := driveapi.Heartbeat{Version: s.cfg.Version, OS: osName(), Machine: s.cfg.Machine, SpoolBytes: &spool,
		Queued: st.Queued + st.Waiting, Held: st.Held, Failed: st.Failed, Full: st.Full}
	if free >= 0 {
		hb.FreeBytes = &free
	}
	if err := s.api.Heartbeat(ctx, hb); err != nil {
		s.log.Warn("heartbeat", "err", err.Error())
	}
}
