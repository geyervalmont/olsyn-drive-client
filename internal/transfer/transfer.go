// Package transfer moves bytes between this PC and the drive. Engine is
// the small interface the file system (vfs) and the send command use;
// Simple is a correct, plain implementation of it (bounded parallel parts,
// resume from the parts the drive reports, fresh part URLs on expiry, a
// restart when the upload session is gone, backoff, pause and a bandwidth
// limit). A more elaborate engine can replace Simple behind Engine.
package transfer

import (
	"context"
	"io"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// Engine uploads and downloads.
type Engine interface {
	// Upload sends Src to the folder ParentID as Name and returns the file.
	// It blocks until done, refused or ctx ends. Errors keep the drive's
	// classification (driveapi.IsPermanent, IsConflict, IsRetryable, …).
	Upload(ctx context.Context, req UploadRequest) (*UploadResult, error)
	// Download writes bytes [Offset, Offset+Length) of a version to Dst at
	// their file offsets, resuming after dropped connections until ctx ends.
	Download(ctx context.Context, req DownloadRequest) error
	// Pause holds every transfer at its next chunk; Resume lets them go.
	Pause()
	Resume()
	Paused() bool
	// SetBandwidth limits bytes per second per direction (0 = unlimited).
	SetBandwidth(up, down int64)
}

// UploadRequest describes one upload.
type UploadRequest struct {
	// TransferID names the transfer in events (e.g. the journal sequence).
	TransferID string
	ParentID   string // drive id of the folder
	Name       string
	Size       int64
	Src        io.ReaderAt
	Mime       string
	// Conflict is driveapi.ConflictFail for new files, ConflictNewVersion
	// with BaseVersionID for changed ones.
	Conflict      string
	BaseVersionID string
	ModifiedAt    time.Time
	// Fingerprint lets the drive hand back an unfinished upload of the same
	// content (≤ 200 chars). Set it: resume depends on it.
	Fingerprint string
	// UploadID is a session started earlier (from the journal). The engine
	// resumes it, or completes it again if it had finished.
	UploadID string
	// PreCheck asks the drive first and skips identical content. (The
	// SHA-256 is always sent at completion: the drive checks files up to
	// 64 MiB at once and records larger ones as claimed until its own pass.)
	PreCheck bool
	// OnSession is called with the drive's upload id as soon as it is
	// known, so the caller can persist it for a resume after a crash.
	OnSession func(uploadID string)
}

// UploadResult is a finished upload.
type UploadResult struct {
	Node    *driveapi.Node
	Skipped bool // pre-check found identical content: nothing sent
	Resumed bool // carried on from parts sent earlier
	Sent    int64
}

// DownloadRequest describes one ranged download.
type DownloadRequest struct {
	TransferID string
	Name       string
	NodeID     string
	VersionID  string
	Offset     int64
	Length     int64
	Dst        io.WriterAt
}

// Direction of a transfer.
type Direction string

// Directions.
const (
	Up   Direction = "up"
	Down Direction = "down"
)

// EventKind says what happened.
type EventKind string

// Event kinds.
const (
	EventStarted  EventKind = "started"
	EventProgress EventKind = "progress"
	EventDone     EventKind = "done"
	EventSkipped  EventKind = "skipped"
	EventFailed   EventKind = "failed"
	EventWaiting  EventKind = "waiting" // retrying after a failure that should pass
)

// Event reports transfer progress. It never carries file content.
type Event struct {
	Kind       EventKind
	Direction  Direction
	TransferID string
	Name       string
	Bytes      int64
	Total      int64
	Err        error
	At         time.Time
}

// BufferAt is an io.WriterAt over a byte slice holding [Base, Base+len).
type BufferAt struct {
	Buf  []byte
	Base int64
}

// WriteAt implements io.WriterAt.
func (b *BufferAt) WriteAt(p []byte, off int64) (int, error) {
	i := off - b.Base
	if i < 0 || i+int64(len(p)) > int64(len(b.Buf)) {
		return 0, io.ErrShortWrite
	}
	return copy(b.Buf[i:], p), nil
}
