// Package vfs is the drive's file system semantics on top of the index,
// cache, journal and transfer engine, with no OS calls: mount adapters
// (WinFsp, later FUSE) translate their callbacks to FileSystem and map Kind
// errors to their own codes. It is tested on any OS against fakedrive.
//
// Rules (docs/design.md, "File system behaviour"):
//   - listings come from the index; a folder not yet listed is fetched once
//     with a bounded wait, then kept fresh by the changes feed;
//   - reads fetch the ranges asked for (with read-ahead) into the cache;
//     files not cached carry the offline attribute;
//   - writes go to the cache; closing a written file queues it in the
//     journal and the engine uploads in the background;
//   - nothing on the request path waits on the network more than a couple
//     of seconds except a read of uncached data (ReadTimeout);
//   - refusals are held with a plain reason, never retried, never lose data;
//   - a file changed on the drive since we opened it is uploaded beside it
//     as a conflict copy.
package vfs

import (
	"context"
	"log/slog"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/index"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

// FileSystem is what a mount adapter calls. Paths are "/"- or
// "\"-separated OS names relative to the drive's root.
type FileSystem interface {
	Stat(ctx context.Context, path string) (Info, error)
	ReadDir(ctx context.Context, path, marker string, limit int) ([]Info, string, error)
	Open(ctx context.Context, path string, flags OpenFlags) (*Handle, error)
	Rename(ctx context.Context, from, to string, replace bool) error
	Remove(ctx context.Context, path string) error
	VolumeInfo(ctx context.Context) Volume
}

var _ FileSystem = (*FS)(nil)

// Access is what an open may do.
type Access uint8

// Access bits.
const (
	AccessRead Access = 1 << iota
	AccessWrite
	AccessDelete
)

// Share is what an open lets others do at the same time (Windows share modes).
type Share uint8

// Share bits.
const (
	ShareRead Share = 1 << iota
	ShareWrite
	ShareDelete
	ShareAll = ShareRead | ShareWrite | ShareDelete
)

// OpenFlags describe an open or create.
type OpenFlags struct {
	Create       bool // create when missing
	Exclusive    bool // fail with KindExists when it exists
	Directory    bool // must be (or create) a folder
	NonDirectory bool // must be a file
	Truncate     bool // empty an existing file (overwrite / supersede)
	Access       Access
	Share        Share
	Attrs        uint32 // OS attributes to keep locally on create/overwrite
}

// Info describes a file or folder.
type Info struct {
	ID       string // stable local id
	Name     string // OS name (mapped)
	IsDir    bool
	Size     int64
	MTime    time.Time
	CTime    time.Time
	Attrs    uint32 // OS attributes stored locally (hidden, …)
	ReadOnly bool   // the drive will not take changes here
	Offline  bool   // content is not on this PC (do not open just to peek)
	Pending  bool   // changes not yet on the drive
}

// Volume is what the drive reports as a disk.
type Volume struct {
	Label string
	Total int64
	Free  int64
}

// State is the connection state shown to the person.
type State string

// States.
const (
	StateOnline    State = "online"
	StateOffline   State = "offline"
	StateSignedOut State = "signed_out"
)

// EventKind says what an Event reports.
type EventKind string

// Event kinds.
const (
	EventTransfer EventKind = "transfer" // Transfer is set
	EventHeld     EventKind = "held"     // a change was refused: see Held
	EventReleased EventKind = "released" // a held change was discarded or saved
	EventConflict EventKind = "conflict" // a conflict copy was made (Path is the copy)
	EventState    EventKind = "state"    // connection state changed
	EventSynced   EventKind = "synced"   // a queued change reached the drive
	EventCache    EventKind = "cache"    // content left the cache (Reason says why)
)

// Event is one thing the agent or UI may show. No file content, no token.
type Event struct {
	Kind     EventKind
	At       time.Time
	Path     string
	Seq      uint64
	State    State
	Reason   string
	Transfer *transfer.Event
}

// Held is a change the drive refused; it waits for the person.
type Held struct {
	Seq    uint64
	Path   string
	Kind   string // journal op kind
	Code   string
	Reason string
	Size   int64
	Since  time.Time
	// HasContent: this PC still has the file's bytes (a copy can be saved).
	HasContent bool
	// CanReupload: the drive damaged what was sent (checksum mismatch) and
	// this PC has the bytes: Reupload sends them again.
	CanReupload bool
}

// Queued is a change waiting for, or on its way to, the drive.
type Queued struct {
	Seq       uint64
	NodeID    string
	Kind      string // journal op kind
	Name      string
	Size      int64
	QueuedAt  time.Time
	Active    bool // running now
	Attempts  int
	LastError string    // a code
	NotBefore time.Time // when it is retried, after a failure
}

// Config wires an FS.
type Config struct {
	API     *driveapi.Client
	Engine  transfer.Engine
	Index   *index.DB
	Cache   *cache.Cache
	Journal *journal.Journal
	// RootID is the drive id of the folder shown as the drive's root.
	RootID string
	// PCName goes into conflict copy names.
	PCName string
	// ListTimeout bounds the wait for a folder's first listing (default 2 s).
	ListTimeout time.Duration
	// ReadTimeout bounds a read of uncached data (default 30 s).
	ReadTimeout time.Duration
	// PollInterval for the changes feed without SSE wake-ups (default 30 s).
	PollInterval time.Duration
	// Workers is how many queued changes run at once (default 4).
	Workers int
	// ReadAhead is fetched past a sequential read (default 4 MiB).
	ReadAhead int64
	// Backoff for queued changes that fail and should pass.
	Backoff driveapi.Backoff
	// VolumeLabel and VolumeTotal are reported to the OS.
	VolumeLabel string
	VolumeTotal int64
	// DisableStream turns off the SSE wake-ups (tests).
	DisableStream bool
	// Release are the timings of releasing uploaded content from the cache
	// once the drive has confirmed it (zero: cache.ReleasePolicy defaults).
	// SetRelease changes them while running.
	Release cache.ReleasePolicy
	// ReleaseInterval is how often the release rules run (default 30 s).
	ReleaseInterval time.Duration
	// KeepOffline reports a drive path (OS names, "/a/b.txt") whose content
	// must stay on this PC: the hook for keep-offline folders. Nil keeps
	// nothing beyond the release rules.
	KeepOffline func(path string) bool
	Logger      *slog.Logger
}
