package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// The interfaces below are everything the agent needs from the rest of the
// client. The real implementations (driveapi, vfs + transfer + journal,
// mount) are adapted to them in cmd/olsyn-drive; tests use in-memory fakes.

// Drive is the part of the drive server API the agent calls itself.
type Drive interface {
	// LinkStart begins a device link (POST /api/mount/v1/link).
	LinkStart(ctx context.Context, req LinkRequest) (Link, error)
	// LinkPoll asks for the token once (POST /api/mount/v1/link/token).
	// It returns ErrAuthorizationPending, ErrLinkDenied, ErrLinkExpired or a
	// *RetryAfterError (slow down) until it succeeds.
	LinkPoll(ctx context.Context, deviceCode string) (LinkResult, error)
	// Heartbeat reports this device (POST /api/mount/v1/heartbeat).
	// ErrUnauthenticated means the token was revoked.
	Heartbeat(ctx context.Context, token string, hb Heartbeat) error
	// Unlink revokes this device and its token (DELETE /api/v1/devices/{id}).
	Unlink(ctx context.Context, token, deviceID string) error
}

// LinkRequest describes this device to the approval page.
type LinkRequest struct {
	Client     string
	Machine    string
	AppVersion string
	Platform   string
}

// Link is a started device link. DeviceCode is secret: never shown or logged.
type Link struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        time.Duration
	ExpiresIn       time.Duration
}

// LinkResult is an approved link.
type LinkResult struct {
	Token    string
	DeviceID string
	ReadOnly bool
	User     User
}

// User is the person who approved the link.
type User struct {
	ID    string
	Name  string
	Email string
}

// Heartbeat is what the device reports.
type Heartbeat struct {
	State      string
	MountPath  string
	ErrorCode  string
	AppVersion string
	Platform   string
}

// Errors a Drive implementation maps its responses onto.
var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrLinkDenied           = errors.New("device link denied")
	ErrLinkExpired          = errors.New("device link expired")
	ErrUnauthenticated      = errors.New("token not accepted by the drive")
)

// RetryAfterError is a "wait, then try again" answer: 503 with Retry-After,
// access_unavailable, writes_paused, or 429.
type RetryAfterError struct {
	Code  string
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("drive asked to wait %s (%s)", e.After, e.Code)
}

// Session is what the file system needs to run for a signed-in account.
type Session struct {
	ServerURL     string
	Token         string
	DeviceID      string
	UserID        string
	ReadOnly      bool
	CacheDir      string
	CacheCapBytes int64
	CacheRelease  settings.CacheRelease
	Bandwidth     settings.Bandwidth
}

// Connectivity values of FSStatus.
const (
	Online           = "online"
	Offline          = "offline"
	ServerRestarting = "server_restarting"
)

// FSStatus is the file system core's current condition.
type FSStatus struct {
	Connectivity string // Online, Offline or ServerRestarting
	Summary      Summary
	CacheFull    bool
	// Unsent counts queued and held changes (cheap: kept up to date).
	Unsent Unsent
	// Cache is how the cache's space is used.
	Cache CacheState
}

// FSEvent is one change from the file system core. Exactly one field is set.
type FSEvent struct {
	Transfer      *Transfer
	Held          *HeldChange
	Notification  *Notification
	Batch         *Batch
	StatusChanged bool
}

// TransferQuery filters GET /v1/transfers.
type TransferQuery struct {
	States []string
	Cursor string
	Limit  int
}

// FileSystem is the core behind the mount: index, cache, journal, transfer
// engine and vfs, as one unit.
type FileSystem interface {
	Start(ctx context.Context, s Session) error
	// Stop flushes open writes into the journal and stops transfers.
	Stop(ctx context.Context) error
	Status() FSStatus
	// Events delivers changes for the life of the FileSystem.
	Events() <-chan FSEvent
	Transfers(ctx context.Context, q TransferQuery) (TransferPage, error)
	Retry(ctx context.Context, id string) (Transfer, error)
	Cancel(ctx context.Context, id string) (Transfer, error)
	DiscardHeld(ctx context.Context, id string) error
	// ReuploadHeld queues a held file again (only a checksum mismatch with
	// the bytes still on this PC; otherwise ErrNotRetryable).
	ReuploadHeld(ctx context.Context, id string) error
	SaveHeldCopy(ctx context.Context, id, destDir string) (string, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	// Send uploads local files and folders into a drive folder as one batch.
	Send(ctx context.Context, req SendRequest) (SendResult, error)
	// Batches lists recent sends with their progress, newest first.
	Batches() []Batch
	// CacheUsage reports the cache's size and unsent bytes (Dir, CapBytes
	// and DiskFreeBytes are filled in by the agent).
	CacheUsage(ctx context.Context) (CacheUsage, error)
	// FreeCache releases every cached file that can go now: not open, not
	// unsent, not an upload the drive has yet to confirm.
	FreeCache(ctx context.Context) (CacheFreed, error)
	// DriveFolders lists the folders in a drive folder, given by path or id.
	DriveFolders(ctx context.Context, parentPath, parentID, cursor string) (DriveFolders, error)
	SetBandwidth(b settings.Bandwidth)
	// SetCacheCap applies a new cache size cap at once.
	SetCacheCap(bytes int64)
	// SetCacheRelease applies new release timings at once.
	SetCacheRelease(r settings.CacheRelease)
	// Unsent counts journal entries and held items; works while stopped.
	Unsent(ctx context.Context) (Unsent, error)
	// ClearCache deletes the cache, unsent data included; only while stopped.
	ClearCache(ctx context.Context, cacheDir string) error
	// WebURL is the drive web app's address for a drive path ("" = the drive's home).
	WebURL(ctx context.Context, drivePath string) (string, error)
}

// Errors a FileSystem returns, mapped onto IPC error codes.
var (
	ErrNotFound            = errors.New("not found")
	ErrNotRetryable        = errors.New("not retryable")
	ErrNotCancellable      = errors.New("not cancellable")
	ErrNotHeld             = errors.New("not held")
	ErrReadOnly            = errors.New("device is read-only")
	ErrLocalNotFound       = errors.New("local path not found")
	ErrDriveFolderNotFound = errors.New("drive folder not found")
	ErrNoUI                = errors.New("this build has no desktop app")
	ErrDriveFolderReadOnly = errors.New("files cannot be added to that drive folder")
	ErrDriveUnreachable    = errors.New("the drive cannot be reached")
)

// Mounter exposes the file system as a drive letter or mount point.
type Mounter interface {
	Mount(ctx context.Context, letter string) (path string, err error)
	Unmount(ctx context.Context, force bool) error
	OpenFiles() int
}

// System is the OS integration the agent uses.
type System interface {
	OpenURL(ctx context.Context, url string) error
	OpenFolder(ctx context.Context, path string) error
	SetAutostart(enabled bool) error
	DetectDriver() (platform.DriverInfo, error)
	// InstallDriver runs the bundled, verified driver installer elevated.
	// progress hears the platform.DriverStep values as they begin.
	InstallDriver(ctx context.Context, progress func(step string)) error
	// DriveLettersInUse maps the letters D: to Z: something holds to what
	// (platform.LetterVolume, LetterNetwork, LetterSubst); nil where the OS
	// has no drive letters.
	DriveLettersInUse() (map[string]string, error)
	DownloadsDir() (string, error)
	FreeDiskBytes(path string) (uint64, error)
	// ShowUI opens the desktop app at a route ("transfers/held"); ErrNoUI
	// when this build has no desktop app.
	ShowUI(ctx context.Context, route string) error
}

// Updater checks for and stages updates (internal/update.Client).
type Updater interface {
	Check(ctx context.Context, channel string) (*update.Release, error)
	Download(ctx context.Context, rel *update.Release, progress func(done, total int64)) (string, error)
}

// Exit tells the caller of Run why the agent stopped.
type Exit struct {
	// Update is set when the agent stopped to apply a staged update.
	Update *PendingUpdate
}

// PendingUpdate is a verified, staged executable to swap in.
type PendingUpdate struct {
	Version    string
	Staged     string
	WasMounted bool
}
