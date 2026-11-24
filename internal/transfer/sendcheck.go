package transfer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// The batch pre-check. Between the walk and the workers, files are gathered
// into groups the drive accepts in one POST /api/v1/uploads/check (at most
// driveapi.CheckMaxFiles files in at most driveapi.CheckMaxFolders folders),
// their folders are made (siblings in parallel), and one call asks which are
// there already. A resend of thousands of files then costs a few dozen
// calls, not one per file, and stays well under the drive's rate limit.
// Files in a folder this run made are not checked at all: nothing is in it.

const (
	// checkFlushAfter: a group is checked once it is full or this long after
	// its first file arrived, so a slow walk still moves.
	checkFlushAfter = time.Second
	// checkBatchBytes caps what one group hashes before its check, so the
	// first uploads of a send of large files do not wait for all of it.
	checkBatchBytes = 4 << 30
	// folderParallel is how many folders of one level are made at once.
	folderParallel = 8
)

// checkStage reads the walk's files from in, pre-checks them in groups and
// hands them to the workers on out, which it closes when in is closed.
// Every file it takes in reaches out or gets a result.
func (s *Sender) checkStage(ctx context.Context, spec SendSpec, folders *folderCache, in <-chan sendJob, out chan<- sendJob, rep SendReport) {
	defer close(out)
	var batch []sendJob
	var size int64
	dirs := map[string]bool{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	flush := func() {
		timer.Stop()
		if len(batch) == 0 {
			return
		}
		s.precheck(ctx, spec, folders, batch, rep)
		for _, j := range batch {
			select {
			case out <- j:
			case <-ctx.Done():
				rep.result(j.f, SendCancelled, "cancelled", "The send was stopped.")
			}
		}
		batch, size = nil, 0
		clear(dirs)
	}
	for {
		var tick <-chan time.Time
		if len(batch) > 0 {
			tick = timer.C
		}
		select {
		case j, ok := <-in:
			if !ok {
				flush()
				return
			}
			dir := relDir(j.rel)
			if !dirs[dir] && len(dirs) == driveapi.CheckMaxFolders {
				flush()
			}
			if len(batch) == 0 {
				timer.Reset(checkFlushAfter)
			}
			batch = append(batch, j)
			dirs[dir] = true
			size += j.f.Size
			if len(batch) == driveapi.CheckMaxFiles || size >= checkBatchBytes {
				flush()
			}
		case <-tick:
			flush()
		}
	}
}

// precheck marks each file of the group: present (the drive has it),
// checked (it does not: upload without asking again), or neither (its
// folder could not be made, it could not be read, or the drive could not
// answer: the worker finds out and reports).
func (s *Sender) precheck(ctx context.Context, spec SendSpec, folders *folderCache, batch []sendJob, rep SendReport) {
	t0 := time.Now()
	var dirs []string
	seen := map[string]bool{}
	for _, j := range batch {
		if d := relDir(j.rel); !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// folders first; while the drive cannot be reached, wait (and say so)
	// rather than hand the files on unchecked
	made, failed := folders.ensureAll(ctx, dirs, folderParallel)
	for attempt := 0; ctx.Err() == nil; attempt++ {
		var again []string
		var why error
		for _, d := range dirs {
			if err := failed[d]; err != nil && transient(err) {
				again, why = append(again, d), err
			}
		}
		if len(again) == 0 || s.waitOut(ctx, rep, batch, jobsIn(batch, again), attempt, why) != nil {
			break
		}
		m, f := folders.ensureAll(ctx, again, folderParallel)
		maps.Copy(made, m)
		failed = f
	}
	folderTime := time.Since(t0)
	var ask []int
	for i := range batch {
		e := made[relDir(batch[i].rel)]
		switch {
		case e == nil: // the worker makes the folder (or reports why not)
		case e.created:
			batch[i].checked = true // this run made the folder: nothing is in it
		default:
			ask = append(ask, i)
		}
	}
	hashJobs(ctx, batch, ask, max(spec.Files, 2))
	var files []driveapi.CheckFile
	var idx []int
	for _, i := range ask {
		j := batch[i]
		if j.sum == "" {
			continue // unreadable or changed: the worker reports it
		}
		files = append(files, driveapi.CheckFile{ParentID: made[relDir(j.rel)].id, Name: path.Base(j.rel), Size: j.f.Size,
			Fingerprint: sendFingerprint(j.f.Local, j.f.Size, j.mt), SHA256: j.sum})
		idx = append(idx, i)
	}
	present := 0
	calls := 0
	for _, g := range s.checkGroups(ctx, rep, batch, files, idx) {
		calls += g.calls
		for k, i := range g.idx {
			if g.res[k].State == driveapi.CheckPresent {
				batch[i].present = true
				present++
			} else {
				batch[i].checked = true
			}
		}
	}
	s.log().Info("send: pre-check", "batch", spec.BatchID, "files", len(batch), "folders", len(dirs),
		"folders_ms", folderTime.Milliseconds(), "asked", len(files), "present", present, "calls", calls,
		"ms", time.Since(t0).Milliseconds())
}

type checkGroup struct {
	idx   []int
	res   []driveapi.CheckResult
	calls int
}

// checkGroups asks the drive about files (whose batch indexes are idx),
// trying again while it cannot answer. When the drive refuses the whole
// call (a folder the person may not upload to), each folder is asked on
// its own so the others are still checked; a folder refused alone is left
// to its workers, whose uploads are refused with the drive's reason.
func (s *Sender) checkGroups(ctx context.Context, rep SendReport, batch []sendJob, files []driveapi.CheckFile, idx []int) []checkGroup {
	if len(files) == 0 {
		return nil
	}
	ask := func(files []driveapi.CheckFile, idx []int) ([]driveapi.CheckResult, error) {
		for attempt := 0; ; attempt++ {
			res, err := s.API.CheckUploads(ctx, files, false)
			if err == nil || !transient(err) || ctx.Err() != nil {
				return res, err
			}
			if werr := s.waitOut(ctx, rep, batch, idx, attempt, err); werr != nil {
				return nil, err
			}
		}
	}
	res, err := ask(files, idx)
	if err == nil {
		return []checkGroup{{idx: idx, res: res, calls: len(driveapi.SplitCheck(files))}}
	}
	if !driveapi.IsPermanent(err) {
		s.log().Warn("send: pre-check failed", "code", errCodeOf(err))
		return nil
	}
	byParent := map[string]*checkGroup{}
	var order []string
	var fs = map[string][]driveapi.CheckFile{}
	for k, f := range files {
		g := byParent[f.ParentID]
		if g == nil {
			g = &checkGroup{}
			byParent[f.ParentID] = g
			order = append(order, f.ParentID)
		}
		g.idx = append(g.idx, idx[k])
		fs[f.ParentID] = append(fs[f.ParentID], f)
	}
	var out []checkGroup
	for _, p := range order {
		g := byParent[p]
		g.calls = 1
		if g.res, err = ask(fs[p], g.idx); err != nil {
			continue
		}
		out = append(out, *g)
	}
	return out
}

// waitOut tells the files at idx that the drive could not be reached (or
// asked to wait), and waits before the next try.
func (s *Sender) waitOut(ctx context.Context, rep SendReport, batch []sendJob, idx []int, attempt int, err error) error {
	d := s.Backoff.Delay(min(attempt, 10), err)
	if rep.Waiting != nil {
		at := time.Now().Add(d)
		for _, i := range idx {
			rep.Waiting(batch[i].f, err, at)
		}
	}
	return sleep(ctx, d)
}

// transient reports an error that passes: the drive unreachable, restarting
// or asking to wait.
func transient(err error) bool {
	return (driveapi.IsRetryable(err) || driveapi.IsNetwork(err)) && !isContextErr(err)
}

// jobsIn returns the indexes of the batch's files in dirs.
func jobsIn(batch []sendJob, dirs []string) []int {
	in := map[string]bool{}
	for _, d := range dirs {
		in[d] = true
	}
	var out []int
	for i, j := range batch {
		if in[relDir(j.rel)] {
			out = append(out, i)
		}
	}
	return out
}

// hashJobs works out the SHA-256 of the files at idx, workers at a time.
// A file that cannot be read, or no longer has the size the walk saw, is
// left without one.
func hashJobs(ctx context.Context, batch []sendJob, idx []int, workers int) {
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, len(idx)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if sum, err := hashFile(ctx, batch[i].f.Local, batch[i].f.Size); err == nil {
					batch[i].sum = sum
				}
			}
		}()
	}
feed:
	for _, i := range idx {
		select {
		case work <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
}

func hashFile(ctx context.Context, name string, size int64) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() != size {
		return "", errors.New("transfer: the file changed size")
	}
	return hashReaderAt(ctx, f, size)
}

func relDir(rel string) string {
	d, _ := path.Split(rel)
	return strings.TrimSuffix(d, "/")
}

var errNotAFolder = errors.New("a file on the drive has the name of a folder being sent")

// folderCache makes the batch's folders on the drive, once each. Callers
// asking for the same folder wait for the one making it; different folders
// are made at the same time.
type folderCache struct {
	api      *driveapi.Client
	root     string
	rootPath string
	mu       sync.Mutex
	dirs     map[string]*folderEntry // rel dir → made (or being made)
}

type folderEntry struct {
	done    chan struct{}
	id      string
	created bool // made by this run (so it held nothing before)
	err     error
}

func newFolderCache(api *driveapi.Client, root, rootPath string) *folderCache {
	e := &folderEntry{done: make(chan struct{}), id: root}
	close(e.done)
	return &folderCache{api: api, root: root, rootPath: rootPath, dirs: map[string]*folderEntry{"": e}}
}

// ensureAll makes dirs and their parents, a level at a time with up to
// parallel folders of a level at once, and returns those it has and, for
// those it has not, why.
func (c *folderCache) ensureAll(ctx context.Context, dirs []string, parallel int) (map[string]*folderEntry, map[string]error) {
	levels := map[int][]string{}
	deepest := 0
	seen := map[string]bool{}
	for _, d := range dirs {
		for p := d; p != "" && !seen[p]; p = relDir(p) {
			seen[p] = true
			depth := strings.Count(p, "/") + 1
			levels[depth] = append(levels[depth], p)
			deepest = max(deepest, depth)
		}
	}
	out := map[string]*folderEntry{}
	failed := map[string]error{}
	if e, err := c.get(ctx, ""); err == nil {
		out[""] = e
	}
	var mu sync.Mutex
	sem := make(chan struct{}, max(parallel, 1))
	for depth := 1; depth <= deepest; depth++ {
		var wg sync.WaitGroup
		for _, d := range levels[depth] {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return out, failed
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				e, err := c.get(ctx, d)
				mu.Lock()
				if err == nil {
					out[d] = e
				} else {
					failed[d] = err
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	return out, failed
}

func (c *folderCache) get(ctx context.Context, rel string) (*folderEntry, error) {
	for {
		c.mu.Lock()
		e, ok := c.dirs[rel]
		if !ok {
			e = &folderEntry{done: make(chan struct{})}
			c.dirs[rel] = e
			c.mu.Unlock()
			c.make(ctx, rel, e)
			if e.err != nil {
				return nil, e.err
			}
			return e, nil
		}
		c.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err == nil {
			return e, nil
		}
		if ctx.Err() != nil || !isContextErr(e.err) {
			return nil, e.err
		}
		// the caller making it was cancelled, not this one: try again
	}
}

func (c *folderCache) make(ctx context.Context, rel string, e *folderEntry) {
	defer close(e.done)
	e.id, e.created, e.err = c.create(ctx, rel)
	if e.err != nil {
		c.mu.Lock()
		if c.dirs[rel] == e {
			delete(c.dirs, rel) // the next caller tries again
		}
		c.mu.Unlock()
	}
}

func (c *folderCache) create(ctx context.Context, rel string) (id string, created bool, err error) {
	parent, name := path.Split(rel)
	pe, err := c.get(ctx, strings.TrimSuffix(parent, "/"))
	if err != nil {
		return "", false, err
	}
	n, err := c.api.CreateFolder(ctx, pe.id, name, driveapi.ConflictFail, time.Time{})
	if driveapi.IsConflict(err) {
		n, err = c.api.ResolvePath(ctx, path.Join(c.rootPath, rel))
		if err != nil {
			return "", false, err
		}
		if !n.IsDir() {
			return "", false, fmt.Errorf("%w: %s", errNotAFolder, path.Join(c.rootPath, rel))
		}
		return n.ID, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return n.ID, true, nil
}

// rootGone reports whether the destination folder no longer exists.
func (c *folderCache) rootGone(ctx context.Context) bool {
	_, err := c.api.GetNode(ctx, c.root)
	return driveapi.IsNotFound(err)
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
