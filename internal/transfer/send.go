package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/localfs"
)

// Sending: local files and folders into a drive folder, through the same
// Engine the mounted drive's uploads use (one pause, one bandwidth limit,
// one set of progress events). Folded in from the relay client's send:
//
//   - the source is only ever read: nothing on this PC is moved, changed
//     or deleted;
//   - cloud "online-only" files (OneDrive Files On-Demand) are not read,
//     because reading them makes the sync client download them; they are
//     reported as not local;
//   - links and junctions are never followed; junk is left out; temp files
//     and names the drive would refuse are reported;
//   - memory stays flat: the walk streams one folder at a time into a short
//     queue, and each file is read in parts;
//   - re-running is safe: the drive's pre-check skips files it already has,
//     and an unfinished upload of the same file resumes (by fingerprint).

// Conflict policies for a name the drive already has with other content.
const (
	OnConflictHold       = "hold"
	OnConflictNewVersion = "new_version"
	OnConflictKeepBoth   = "keep_both"
)

// DateNotKeptReason says, in plain words, that a file was sent without its
// date.
const DateNotKeptReason = "Sent, but without its date: the drive does not accept the date this file has, so it shows when it was uploaded instead."

// Send outcomes.
const (
	SendDone      = "done"      // on the drive
	SendSkipped   = "skipped"   // the drive already has it, or it is not sent (temp file, link, …)
	SendHeld      = "held"      // the drive refused it, or the name is taken: a person decides
	SendFailed    = "failed"    // could not be read, or changed while being sent
	SendNotLocal  = "not_local" // online-only: not downloaded
	SendCancelled = "cancelled"
)

// SendSpec is one batch.
type SendSpec struct {
	BatchID    string
	Sources    []string // absolute paths of files and folders
	ParentID   string   // the drive folder they go into
	ParentPath string   // its drive path
	// Contents sends what is inside a single source folder straight into
	// ParentID instead of a folder of the same name.
	Contents          bool
	Conflict          string // OnConflictHold (default), OnConflictNewVersion, OnConflictKeepBoth
	IncludeOnlineOnly bool
	Files             int // files at once (default 4)
	// Attrs replaces localfs.AttrsOf (tests).
	Attrs func(string, os.FileInfo) localfs.Attrs
	// Skip leaves out local files the person cancelled in an earlier run
	// of this batch (when it carries on after a restart).
	Skip func(local string) bool
}

// SendFile is one file of a batch.
type SendFile struct {
	ID        string // transfer id, unique within the batch: <batch>_<n>
	Local     string
	DrivePath string
	Size      int64
}

// SendReport receives a batch's news, from several goroutines at once.
// Every field is optional.
type SendReport struct {
	// Found: a file will be sent (the scan found it).
	Found func(f SendFile)
	// Start: the file's upload begins; the returned context, when not nil,
	// bounds this one file (a cancel of the file).
	Start func(ctx context.Context, f SendFile) context.Context
	// Waiting: the drive could not be reached or asked to wait; the file
	// is tried again at retryAt.
	Waiting func(f SendFile, err error, retryAt time.Time)
	// Result: the file's outcome. code is the drive's refusal code for
	// held files ("name_conflict", "sealed", …), else a short reason code.
	Result func(f SendFile, outcome, code, reason string)
	// Scanned: the walk has finished (totals are known).
	Scanned func()
}

// Sender runs batches.
type Sender struct {
	API     *driveapi.Client
	Engine  Engine
	Backoff driveapi.Backoff
	Logger  *slog.Logger
}

// ErrSendStopped wraps a run-wide stop: the sign-in was refused, or the
// destination folder went away.
var ErrSendStopped = errors.New("transfer: send stopped")

// ErrSendCancelled is the cause to cancel a run's context with when the
// person cancels the whole batch: unfinished uploads are aborted on the
// drive (a run stopped any other way leaves them to resume).
var ErrSendCancelled = errors.New("transfer: send cancelled")

type sendJob struct {
	f   SendFile
	rel string // below the destination
	mt  time.Time
	// what the batch pre-check found (sendcheck.go)
	present bool   // the drive has this file already
	checked bool   // the drive does not: upload without asking again
	sum     string // its SHA-256, when worked out for the pre-check
}

// Send runs one batch to the end: every file found gets a Result. The
// error is a run-wide stop (cancelled, signed out, destination gone).
func (s *Sender) Send(ctx context.Context, spec SendSpec, rep SendReport) error {
	if spec.Files <= 0 {
		spec.Files = 4
	}
	if spec.Conflict == "" {
		spec.Conflict = OnConflictHold
	}
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	folders := newFolderCache(s.API, spec.ParentID, spec.ParentPath)
	pending := make(chan sendJob, spec.Files*2)
	jobs := make(chan sendJob, spec.Files*2)
	go s.checkStage(ctx, spec, folders, pending, jobs, rep)
	var wg sync.WaitGroup
	for range spec.Files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					rep.result(j.f, SendCancelled, "cancelled", "The send was stopped.")
					continue
				}
				if err := s.sendOne(ctx, spec, folders, j, rep); err != nil {
					stop(err)
				}
			}
		}()
	}
	n := 0
	next := func(rel, local string, size int64, mt time.Time) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		n++
		if spec.Skip != nil && spec.Skip(local) {
			return nil
		}
		f := SendFile{ID: spec.BatchID + "_" + strconv.Itoa(n), Local: local, DrivePath: path.Join(spec.ParentPath, rel), Size: size}
		rep.found(f)
		select {
		case pending <- sendJob{f: f, rel: rel, mt: mt}:
			return nil
		case <-ctx.Done():
			// found but never queued (the queue was full): it still gets
			// its result
			rep.result(f, SendCancelled, "cancelled", "The send was stopped.")
			return context.Cause(ctx)
		}
	}
	skipped := func(rel string, sk localfs.Skip) {
		n++
		f := SendFile{ID: spec.BatchID + "_" + strconv.Itoa(n), DrivePath: path.Join(spec.ParentPath, rel), Size: sk.Size}
		rep.found(f)
		if sk.NotLocal {
			rep.result(f, SendNotLocal, "not_downloaded", "Online-only: "+sk.Reason)
		} else {
			rep.result(f, SendSkipped, "not_sent", sk.Reason)
		}
	}
	var walkErr error
	for _, src := range spec.Sources {
		if walkErr = s.walkSource(ctx, spec, src, next, skipped); walkErr != nil {
			break
		}
	}
	close(pending)
	wg.Wait()
	if rep.Scanned != nil {
		rep.Scanned()
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		log.Warn("send: walk stopped", "batch", spec.BatchID, "err", walkErr)
		return walkErr
	}
	return ctx.Err()
}

func (s *Sender) walkSource(ctx context.Context, spec SendSpec, src string, next func(rel, local string, size int64, mt time.Time) error,
	skipped func(string, localfs.Skip)) error {
	st, err := os.Lstat(src)
	if err != nil {
		skipped(filepath.Base(src), localfs.Skip{Reason: "can't be read: " + err.Error()})
		return nil
	}
	name := filepath.Base(src)
	if why := localfs.CheckName(name); why != "" && !(st.IsDir() && spec.Contents) {
		skipped(name, localfs.Skip{Reason: why})
		return nil
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			skipped(name, localfs.Skip{Reason: "not a regular file (links are never followed)"})
			return nil
		}
		attrs := spec.Attrs
		if attrs == nil {
			attrs = localfs.AttrsOf
		}
		if a := attrs(src, st); a.Placeholder && !spec.IncludeOnlineOnly {
			skipped(name, localfs.Skip{NotLocal: true, Size: st.Size(), Reason: "make it available offline first"})
			return nil
		}
		return next(name, src, st.Size(), st.ModTime())
	}
	prefix := name + "/"
	if spec.Contents {
		prefix = ""
	}
	return localfs.Walk(ctx, src, localfs.WalkOptions{IncludeOnlineOnly: spec.IncludeOnlineOnly, Attrs: spec.Attrs},
		func(e localfs.Entry) error {
			return next(prefix+e.Rel, e.Abs, e.Size, time.Unix(0, e.MtimeNs))
		},
		func(sk localfs.Skip) { skipped(prefix+sk.Rel, sk) })
}

// sendOne uploads one file, trying again while the drive cannot be
// reached. Its error is a run-wide stop.
func (s *Sender) sendOne(ctx context.Context, spec SendSpec, folders *folderCache, j sendJob, rep SendReport) error {
	fctx := ctx
	if rep.Start != nil {
		if c := rep.Start(ctx, j.f); c != nil {
			fctx = c
		}
	}
	for attempt := 0; ; attempt++ {
		if fctx.Err() != nil { // cancelled while queued, paused or waiting to try again
			if ctx.Err() != nil && !errors.Is(context.Cause(ctx), ErrSendCancelled) {
				rep.result(j.f, SendCancelled, "cancelled", "The send was stopped; send again to carry on.")
			} else {
				rep.result(j.f, SendCancelled, "cancelled", "Cancelled.")
			}
			return nil
		}
		outcome, code, reason, err := s.tryOne(fctx, ctx, spec, folders, j, attempt)
		switch {
		case err == nil:
			rep.result(j.f, outcome, code, reason)
			return nil
		case ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrSendCancelled):
			rep.result(j.f, SendCancelled, "cancelled", "Cancelled.")
			return nil
		case ctx.Err() != nil:
			rep.result(j.f, SendCancelled, "cancelled", "The send was stopped; send again to carry on.")
			return nil
		case fctx.Err() != nil:
			rep.result(j.f, SendCancelled, "cancelled", "Cancelled.")
			return nil
		case driveapi.IsAuthLost(err):
			rep.result(j.f, SendCancelled, "signed_out", "This PC was signed out.")
			return fmt.Errorf("%w: %w", ErrSendStopped, err)
		case driveapi.IsNotFound(err) && folders.rootGone(ctx):
			rep.result(j.f, SendFailed, "drive_folder_not_found", "The drive folder this was going to is gone.")
			return fmt.Errorf("%w: the drive folder is gone", ErrSendStopped)
		case driveapi.IsRetryable(err) || driveapi.IsNetwork(err):
			d := s.Backoff.Delay(min(attempt, 10), err)
			if rep.Waiting != nil {
				rep.Waiting(j.f, err, time.Now().Add(d))
			}
			t := time.NewTimer(d)
			select {
			case <-fctx.Done():
				t.Stop()
			case <-t.C:
			}
		default:
			rep.result(j.f, SendFailed, "error", driveapi.Reason(err))
			return nil
		}
	}
}

// tryOne makes one attempt. A nil error comes with the final outcome.
// ctx bounds this file; run is the batch's. An upload the person cancelled
// (this file, or the whole batch) is aborted on the drive, so nothing of it
// is kept there; one stopped with the session stays to be resumed. The
// batch pre-check's answer stands for the first attempt; a later one asks
// the drive again (an earlier attempt may have got the file there).
func (s *Sender) tryOne(ctx, run context.Context, spec SendSpec, folders *folderCache, j sendJob, attempt int) (outcome, code, reason string, err error) {
	if j.present && attempt == 0 {
		return SendSkipped, "already_there", "The drive already has this file.", nil
	}
	var session, startedParent, startedFP string
	defer func() {
		cancelled := ctx.Err() != nil && (run.Err() == nil || errors.Is(context.Cause(run), ErrSendCancelled))
		if err == nil || !cancelled || (session == "" && startedFP == "") {
			return
		}
		actx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		if session == "" {
			// cancelled while the start was on its way: the drive may have
			// made the session without our hearing its id
			if session = s.findSession(actx, startedParent, startedFP); session == "" {
				return
			}
		}
		if aerr := s.API.AbortUpload(actx, session); aerr != nil && !driveapi.IsNotFound(aerr) {
			s.log().Warn("send: abort a cancelled upload", "transfer", j.f.ID, "code", errCodeOf(aerr))
		}
	}()
	name := path.Base(j.rel)
	folder, err := folders.get(ctx, relDir(j.rel))
	if err != nil {
		if driveapi.IsPermanent(err) || errors.Is(err, errNotAFolder) {
			return refusal(err)
		}
		return "", "", "", err
	}
	f, err := os.Open(j.f.Local)
	if err != nil {
		return SendFailed, "unreadable", "Can't be read: " + err.Error(), nil
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return SendFailed, "unreadable", "Can't be read: " + err.Error(), nil
	}
	req := UploadRequest{TransferID: j.f.ID, ParentID: folder.id, Name: name, Size: before.Size(), Src: f,
		Mime: mime.TypeByExtension(path.Ext(name)), Conflict: driveapi.ConflictFail, ModifiedAt: before.ModTime(),
		Fingerprint: sendFingerprint(j.f.Local, before.Size(), before.ModTime()), PreCheck: attempt > 0 || !(j.checked || folder.created),
		OnSession: func(id string) { session = id }}
	if before.Size() == j.f.Size && before.ModTime().Equal(j.mt) {
		req.SHA256 = j.sum // hashed for the pre-check, and unchanged since
	}
	startedParent, startedFP = req.ParentID, req.Fingerprint
	res, err := s.Engine.Upload(ctx, req)
	if driveapi.IsConflict(err) {
		switch spec.Conflict {
		case OnConflictNewVersion:
			var existing *driveapi.Node
			existing, err = s.API.ResolvePath(ctx, j.f.DrivePath)
			if err == nil {
				req.Conflict, req.BaseVersionID = driveapi.ConflictNewVersion, existing.VersionID()
				res, err = s.Engine.Upload(ctx, req)
			}
		case OnConflictKeepBoth:
			req.Conflict = driveapi.ConflictRename
			res, err = s.Engine.Upload(ctx, req)
		default:
			return SendHeld, driveapi.CodeNameConflict,
				"The drive already has a different file named " + name + " here. It was left as it is; this file was not sent.", nil
		}
	}
	if err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			return SendHeld, driveapi.CodeChecksumMismatch, driveapi.ChecksumMismatchReason, nil
		}
		if driveapi.IsPermanent(err) || driveapi.IsConflict(err) {
			return refusal(err)
		}
		return "", "", "", err
	}
	if after, serr := os.Stat(j.f.Local); serr != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return SendFailed, "changed", "The file changed while it was being sent. Send it again when it is finished.", nil
	}
	if res.Skipped {
		return SendSkipped, "already_there", "The drive already has this file.", nil
	}
	if res.DateNotKept {
		return SendDone, NoteDateNotKept, DateNotKeptReason, nil
	}
	return SendDone, "", "", nil
}

// findSession returns the id of the caller's unfinished upload into parent
// with this fingerprint, or "".
func (s *Sender) findSession(ctx context.Context, parent, fingerprint string) string {
	ups, err := s.API.ListUploads(ctx)
	if err != nil {
		s.log().Warn("send: list unfinished uploads", "code", errCodeOf(err))
		return ""
	}
	for _, u := range ups {
		if u.ParentID == parent && u.Fingerprint == fingerprint {
			return u.UploadID
		}
	}
	return ""
}

func (s *Sender) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func refusal(err error) (string, string, string, error) {
	code := "refused"
	if e, ok := driveapi.AsError(err); ok && e.Code != "" {
		code = e.Code
	}
	if errors.Is(err, errNotAFolder) {
		code = "not_a_folder"
	}
	return SendHeld, code, driveapi.Reason(err), nil
}

// sendFingerprint names an unchanged local file to the drive, so a second
// run hands back the first run's unfinished upload.
func sendFingerprint(abs string, size int64, mt time.Time) string {
	h := sha256.Sum256([]byte(filepath.ToSlash(abs)))
	return fmt.Sprintf("olsyn-drive-send/1|%d|%d|%s", size, mt.UnixNano(), hex.EncodeToString(h[:8]))
}

func (r SendReport) found(f SendFile) {
	if r.Found != nil {
		r.Found(f)
	}
}

func (r SendReport) result(f SendFile, outcome, code, reason string) {
	if r.Result != nil {
		r.Result(f, outcome, code, reason)
	}
}
