package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/state"
)

// Item is one local file for a Run: where it is and where it goes.
type Item struct {
	Abs     string
	Rel     string // "/"-separated below the source root; folders are made to match
	Size    int64
	MtimeNs int64
}

// Plan outcomes reported only by a dry run.
const (
	WouldSend   = "would_send"
	WouldUpdate = "would_update" // new_version / keep_both on a name the drive has
	Pending     = "pending"      // spool: sent, the drive hasn't hashed it yet
)

// RunOptions set up a Run.
type RunOptions struct {
	Dest      string // drive folder id the source root maps to
	Person    string // on_behalf_of (a shared node); "" for a personal node
	Assurance string
	Conflict  string // hold (default) | new_version | keep_both
	Files     int    // files in flight at once (default 4)
	DryRun    bool
	// Verify: a file is done only when the drive's own hash matches ours
	// (a spool deletes only then). A send reports "sent" without waiting.
	Verify bool
	// KeyScope separates records of different roots/destinations.
	KeyScope string
	// OnResult is told each item's outcome (from several goroutines).
	OnResult func(Item, Result)
}

// Run sends items as they're added, checking up to MaxCheck at a time
// and sending Files at once. Memory is bounded by the queues, not by how
// many files there are.
type Run struct {
	e       *Engine
	opt     RunOptions
	in      chan Item
	work    chan job
	wg      sync.WaitGroup
	ctx     context.Context
	mu      sync.Mutex
	folders map[string]string // rel dir → drive folder id ("" = doesn't exist, dry run)
	err     error

	sendersDone *sync.WaitGroup
}

type job struct {
	it       Item
	rec      *state.Record
	conflict string
}

// Start begins a run; Add items, then Wait.
func (e *Engine) Start(ctx context.Context, opt RunOptions) *Run {
	if opt.Files <= 0 {
		opt.Files = 4
	}
	if opt.Conflict == "" {
		opt.Conflict = OnConflictHold
	}
	r := &Run{e: e, opt: opt, ctx: ctx, in: make(chan Item, 64), work: make(chan job, opt.Files), folders: map[string]string{"": opt.Dest}}
	r.wg.Add(1)
	go r.checker()
	var senders sync.WaitGroup
	for range opt.Files {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for j := range r.work {
				r.send(j)
			}
		}()
	}
	go func() { r.wg.Wait(); close(r.work) }()
	r.sendersDone = &senders
	return r
}

// Add queues one item (blocks while the queues are full).
func (r *Run) Add(it Item) error {
	select {
	case r.in <- it:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

// Wait finishes the run: every added item has a result. The error is a
// run-wide stop (sign-in refused, cancelled), not a file's failure.
func (r *Run) Wait() error {
	close(r.in)
	r.sendersDone.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Run) stop(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (r *Run) report(it Item, res Result) {
	if r.opt.OnResult != nil {
		r.opt.OnResult(it, res)
	}
}

func (r *Run) key(it Item) string { return state.KeyFor(r.opt.KeyScope, it.Rel) }

// checker batches items: up to MaxCheck, or whatever has arrived within
// a short wait, so a slow trickle (a spool) doesn't wait for a full batch.
func (r *Run) checker() {
	defer r.wg.Done()
	var batch []Item
	flush := func() {
		if len(batch) > 0 {
			r.checkBatch(batch)
			batch = batch[:0]
		}
	}
	for {
		var it Item
		var ok bool
		if len(batch) == 0 {
			it, ok = <-r.in
		} else {
			select {
			case it, ok = <-r.in:
			case <-time.After(200 * time.Millisecond):
				flush()
				continue
			}
		}
		if !ok {
			flush()
			return
		}
		batch = append(batch, it)
		if len(batch) >= driveapi.MaxCheck {
			flush()
		}
	}
}

// record loads (or starts) an item's record; one for an older version of
// the file (other size or time) starts over.
func (r *Run) record(it Item) (*state.Record, error) {
	key := r.key(it)
	rec, err := r.e.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if rec != nil && (rec.Size != it.Size || rec.MtimeNs != it.MtimeNs || rec.Person != r.opt.Person) {
		rec = nil
	}
	if rec == nil || rec.State == state.Failed || rec.State == state.Held {
		rec = &state.Record{Key: key, Rel: it.Rel, Size: it.Size, MtimeNs: it.MtimeNs, State: state.Ready,
			Since: time.Now().UTC(), Fingerprint: Fingerprint(it.Rel, it.Size, it.MtimeNs), Person: r.opt.Person, Assurance: r.opt.Assurance}
	}
	return rec, nil
}

// folder finds (or, unless a dry run, makes) the drive folder for a
// relative directory, as the person.
func (r *Run) folder(dir string) (string, bool, error) {
	r.mu.Lock()
	id, ok := r.folders[dir]
	r.mu.Unlock()
	if ok {
		return id, id != "", nil
	}
	var f *driveapi.Folder
	err := r.e.Retry(r.ctx, "folder "+dir, func() error {
		var err error
		f, err = r.e.API.EnsureFolders(r.ctx, r.opt.Dest, dir, r.opt.Person, r.opt.Assurance, r.opt.DryRun)
		return err
	})
	if r.opt.DryRun && driveapi.StatusOf(err) == 404 {
		r.mu.Lock()
		r.folders[dir] = ""
		r.mu.Unlock()
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	r.mu.Lock()
	r.folders[dir] = f.ID
	r.mu.Unlock()
	return f.ID, true, nil
}

func (r *Run) checkBatch(items []Item) {
	type checked struct {
		it  Item
		rec *state.Record
		i   int // index into ins, -1 when not checked
	}
	var cs []checked
	var ins []driveapi.CheckIn
	for _, it := range items {
		if r.ctx.Err() != nil {
			r.report(it, Result{Outcome: Failed, Reason: "stopped"})
			continue
		}
		rec, err := r.record(it)
		if err != nil {
			r.report(it, Result{Outcome: Failed, Reason: err.Error()})
			continue
		}
		dir, name := path.Split(it.Rel)
		dir = strings.TrimSuffix(dir, "/")
		parent, exists, err := r.folder(dir)
		if err != nil {
			res, ferr := refusal(err, dir)
			if ferr != nil {
				res = Result{Outcome: Failed, Reason: ferr.Error()}
			}
			if res.Reason == "" {
				res.Reason = "the folder can't be made"
			}
			res.Reason = fmt.Sprintf("%s (folder %q)", res.Reason, dir)
			r.report(it, res)
			if driveapi.StatusOf(err) == 401 {
				r.stop(ferr)
			}
			continue
		}
		rec.ParentID, rec.Name = parent, name
		// Part-way through (a restart): resume it without a check. A sent
		// file waiting for its hash only matters when verifying.
		mid := rec.State == state.Starting || rec.State == state.Sending || rec.State == state.Completing ||
			(rec.State == state.Verifying && r.opt.Verify)
		switch {
		case !exists: // dry run, folder not there yet
			r.report(it, Result{Outcome: WouldSend})
		case mid && rec.ParentID != "" && rec.UploadID != "":
			cs = append(cs, checked{it, rec, -1}) // resume: no check needed
		default:
			cs = append(cs, checked{it, rec, len(ins)})
			ins = append(ins, driveapi.CheckIn{ParentID: parent, Name: name, Size: it.Size, Fingerprint: rec.Fingerprint})
		}
	}
	var outs []driveapi.CheckOut
	if len(ins) > 0 {
		err := r.e.Retry(r.ctx, "check", func() error {
			var err error
			outs, err = r.e.API.Check(r.ctx, r.opt.Person, r.opt.Assurance, ins)
			return err
		})
		if err != nil {
			reason := err.Error()
			if driveapi.StatusOf(err) == 401 {
				r.stop(fmt.Errorf("the drive no longer accepts this computer's sign-in (run olsyn-drive login): %w", err))
			}
			for _, c := range cs {
				if c.i >= 0 {
					r.report(c.it, Result{Outcome: Failed, Reason: "check: " + reason})
				}
			}
			outs = nil
			cs = slicesFilter(cs, func(c checked) bool { return c.i < 0 })
		}
	}
	for _, c := range cs {
		conflict := ""
		if c.i >= 0 {
			o := outs[c.i]
			res, conf, send := r.decide(c.it, c.rec, o)
			if !send {
				r.report(c.it, res)
				continue
			}
			conflict = conf
		}
		if r.opt.DryRun {
			if conflict != "" {
				r.report(c.it, Result{Outcome: WouldUpdate, Reason: "the drive has a different file of that name: " + conflictWords(conflict)})
			} else {
				r.report(c.it, Result{Outcome: WouldSend})
			}
			continue
		}
		select {
		case r.work <- job{it: c.it, rec: c.rec, conflict: conflict}:
		case <-r.ctx.Done():
			r.report(c.it, Result{Outcome: Failed, Reason: "stopped"})
		}
	}
}

func slicesFilter[T any](s []T, keep func(T) bool) []T {
	out := s[:0]
	for _, v := range s {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func conflictWords(c string) string {
	switch c {
	case OnConflictNewVersion:
		return "it would become a new version"
	case OnConflictKeepBoth:
		return "both would be kept, this one renamed"
	}
	return c
}

// decide turns a check answer into an outcome or a send (with the
// conflict mode to send under).
func (r *Run) decide(it Item, rec *state.Record, o driveapi.CheckOut) (Result, string, bool) {
	same := false
	switch o.State {
	case "absent":
		return Result{}, "", true
	case "present":
		same = true
	case "different":
		if o.SameSize && o.SHA256 != "" {
			// Same name and size, other fingerprint (uploaded another way):
			// compare contents.
			sum, err := r.localHash(it, rec)
			if err != nil {
				return Result{Outcome: Failed, Reason: err.Error()}, "", false
			}
			same = strings.EqualFold(sum, o.SHA256)
		}
	default:
		return Result{Outcome: Failed, Reason: "the drive answered the check with " + o.State}, "", false
	}
	if same {
		if r.opt.Verify && !r.opt.DryRun {
			// Deleting a spooled copy needs the drive's hash to equal ours,
			// not just a matching fingerprint.
			if o.SHA256 == "" {
				return Result{Outcome: Pending, Reason: "the drive hasn't checked its copy yet"}, "", false
			}
			sum, err := r.localHash(it, rec)
			if err != nil {
				return Result{Outcome: Failed, Reason: err.Error()}, "", false
			}
			if !strings.EqualFold(sum, o.SHA256) {
				return r.conflictOr(it, rec)
			}
		}
		rec.Outcome, rec.NodeID, rec.Reason = Already, o.ID, ""
		if o.SHA256 != "" && rec.SHA256 == "" {
			rec.SHA256 = o.SHA256
		}
		if !r.opt.DryRun {
			rec.State = state.Done
			if err := r.e.Store.Put(rec); err != nil {
				return Result{Outcome: Failed, Reason: err.Error()}, "", false
			}
		}
		return Result{Outcome: Already}, "", false
	}
	return r.conflictOr(it, rec)
}

func (r *Run) conflictOr(it Item, rec *state.Record) (Result, string, bool) {
	switch r.opt.Conflict {
	case OnConflictNewVersion, OnConflictKeepBoth:
		return Result{}, r.opt.Conflict, true
	}
	res := Result{Outcome: Held, Reason: fmt.Sprintf("a different file named %q is already there (conflict policy: hold)", rec.Name)}
	if !r.opt.DryRun {
		rec.State, rec.Reason = state.Held, res.Reason
		_ = r.e.Store.Put(rec)
	}
	return res, "", false
}

func (r *Run) localHash(it Item, rec *state.Record) (string, error) {
	if rec.SHA256 != "" {
		return rec.SHA256, nil
	}
	sum, err := HashFile(it.Abs)
	if err != nil {
		return "", err
	}
	rec.SHA256 = sum
	return sum, nil
}

func (r *Run) send(j job) {
	if r.ctx.Err() != nil {
		r.report(j.it, Result{Outcome: Failed, Reason: "stopped"})
		return
	}
	res, err := r.e.Send(r.ctx, Job{Abs: j.it.Abs, Rec: j.rec, Conflict: j.conflict})
	if err != nil {
		reason := err.Error()
		switch {
		case errors.Is(err, ErrChanged):
			reason = "the file changed while it was being sent; it will be sent again when it stops changing"
		case errors.Is(err, context.Canceled):
			reason = "stopped; run again to resume"
		case strings.Contains(reason, "olsyn-drive login"):
			r.stop(err)
		}
		r.report(j.it, Result{Outcome: Failed, Reason: reason})
		return
	}
	if res.Outcome == Held || res.Outcome == Failed {
		j.rec.State, j.rec.Reason = map[string]string{Held: state.Held, Failed: state.Failed}[res.Outcome], res.Reason
		_ = r.e.Store.Put(j.rec)
		r.report(j.it, res)
		return
	}
	if r.opt.Verify {
		done, pending, vres, err := r.e.Verify(r.ctx, j.rec)
		switch {
		case err != nil:
			r.report(j.it, Result{Outcome: Pending, Reason: "sent; checking it later: " + err.Error()})
		case pending:
			r.report(j.it, Result{Outcome: Pending, Reason: "sent; the drive hasn't checked its copy yet"})
		case done:
			r.report(j.it, Result{Outcome: Sent})
		default:
			r.report(j.it, vres)
		}
		return
	}
	r.report(j.it, res)
}
