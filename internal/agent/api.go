package agent

import (
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// APIVersion is the IPC major version this agent serves (docs/ipc.md).
const APIVersion = 1

// Connection values of State.Connection, highest priority first.
const (
	ConnSignedOut        = "signed_out"
	ConnLinking          = "linking"
	ConnStarting         = "starting"
	ConnOffline          = "offline"
	ConnServerRestarting = "server_restarting"
	ConnAttention        = "attention"
	ConnPaused           = "paused"
	ConnWorking          = "working"
	ConnConnected        = "connected"
)

// Mount states.
const (
	MountUnmounted  = "unmounted"
	MountMounting   = "mounting"
	MountMounted    = "mounted"
	MountUnmounting = "unmounting"
	MountFailed     = "failed"
)

// Transfer states.
const (
	TransferActive    = "active"
	TransferWaiting   = "waiting"
	TransferHeld      = "held"
	TransferDone      = "done"
	TransferSkipped   = "skipped"
	TransferFailed    = "failed"
	TransferCancelled = "cancelled"
)

// Problem is a code with a sentence a person can read.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Issue is something that needs the person, and the action the UI offers.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Action  string `json:"action"`
}

// Account is who is signed in on this device. It is not secret; the token is
// kept apart, in the OS credential store.
type Account struct {
	Server   string    `json:"server"`
	UserID   string    `json:"user_id"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	DeviceID string    `json:"device_id"`
	ReadOnly bool      `json:"read_only"`
	LinkedAt time.Time `json:"linked_at"`
}

// SignIn is the device-link progress.
type SignIn struct {
	State           string    `json:"state"`
	UserCode        string    `json:"user_code"`
	VerificationURL string    `json:"verification_url"`
	ExpiresAt       time.Time `json:"expires_at"`
	Error           *Problem  `json:"error"`
}

// Mount is the drive letter / mount point state.
type Mount struct {
	State  string   `json:"state"`
	Letter *string  `json:"letter"`
	Path   string   `json:"path"`
	Error  *Problem `json:"error"`
	// LetterConfirmed is false until the person has chosen the letter
	// (first run): the agent does not mount before that.
	LetterConfirmed bool `json:"letter_confirmed"`
}

// Summary is the overall transfer progress.
type Summary struct {
	Active     int    `json:"active"`
	Waiting    int    `json:"waiting"`
	Held       int    `json:"held"`
	FilesLeft  int    `json:"files_left"`
	BytesTotal int64  `json:"bytes_total"`
	BytesDone  int64  `json:"bytes_done"`
	UpBPS      int64  `json:"up_bps"`
	DownBPS    int64  `json:"down_bps"`
	ETASeconds *int64 `json:"eta_seconds"`
}

// TransferError is a transfer's last error.
type TransferError struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	RetryAt *time.Time `json:"retry_at"`
}

// Held explains why the drive refused a file for good.
type Held struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Since   time.Time `json:"since"`
	Actions []string  `json:"actions"`
}

// TransferCan says which actions would succeed now.
type TransferCan struct {
	Retry  bool `json:"retry"`
	Cancel bool `json:"cancel"`
}

// Transfer is one upload or download.
type Transfer struct {
	ID            string         `json:"id"`
	Direction     string         `json:"direction"`
	Origin        string         `json:"origin"`
	State         string         `json:"state"`
	Name          string         `json:"name"`
	LocalPath     string         `json:"local_path"`
	DrivePath     string         `json:"drive_path"`
	NodeID        *string        `json:"node_id"`
	Size          int64          `json:"size"`
	BytesDone     int64          `json:"bytes_done"`
	BPS           int64          `json:"bps"`
	ETASeconds    *int64         `json:"eta_seconds"`
	Attempts      int            `json:"attempts"`
	QueuedAt      time.Time      `json:"queued_at"`
	StartedAt     *time.Time     `json:"started_at"`
	FinishedAt    *time.Time     `json:"finished_at"`
	Error         *TransferError `json:"error"`
	Held          *Held          `json:"held"`
	SkippedReason *string        `json:"skipped_reason"`
	ConflictCopy  *string        `json:"conflict_copy"`
	BatchID       *string        `json:"batch_id"`
	Can           TransferCan    `json:"can"`
}

// Terminal reports whether the transfer will not change on its own any more.
func (t Transfer) Terminal() bool {
	switch t.State {
	case TransferDone, TransferSkipped, TransferFailed, TransferCancelled, TransferHeld:
		return true
	}
	return false
}

// TransferCounts counts transfers by state.
type TransferCounts struct {
	Active  int `json:"active"`
	Waiting int `json:"waiting"`
	Held    int `json:"held"`
	Done    int `json:"done"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// TransferPage is one page of GET /v1/transfers.
type TransferPage struct {
	Items      []Transfer     `json:"items"`
	NextCursor *string        `json:"next_cursor"`
	Counts     TransferCounts `json:"counts"`
}

// DriverRequirement is a driver version an update needs.
type DriverRequirement struct {
	Name       string `json:"name"`
	MinVersion string `json:"min_version"`
}

// Progress is bytes done of a total.
type Progress struct {
	BytesDone  int64 `json:"bytes_done"`
	BytesTotal int64 `json:"bytes_total"`
}

// Update is the self-update state.
type Update struct {
	State          string             `json:"state"`
	Channel        string             `json:"channel"`
	Current        string             `json:"current"`
	Available      *string            `json:"available"`
	Notes          *string            `json:"notes"`
	PublishedAt    *time.Time         `json:"published_at"`
	Required       bool               `json:"required"`
	DriverRequired *DriverRequirement `json:"driver_required"`
	Progress       *Progress          `json:"progress"`
	Error          *Problem           `json:"error"`
	CheckedAt      *time.Time         `json:"checked_at"`
	Apply          string             `json:"apply"`
}

// Driver is the file system driver state.
type Driver struct {
	State      string   `json:"state"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	MinVersion string   `json:"min_version"`
	Error      *Problem `json:"error"`
	// Progress is the install step while State is "installing":
	// preparing, approval (the administrator prompt is up) or installing.
	Progress string `json:"progress,omitempty"`
	// Others names other file system providers found (e.g. "dokany");
	// they coexist and are only reported.
	Others []string `json:"others,omitempty"`
}

// State is GET /v1/state and the state event.
type State struct {
	APIVersion  int        `json:"api_version"`
	APIVersions []int      `json:"api_versions"`
	AgentRun    string     `json:"agent_run"`
	Version     string     `json:"version"`
	Platform    string     `json:"platform"`
	Ready       bool       `json:"ready"`
	Connection  string     `json:"connection"`
	Reason      *Problem   `json:"reason"`
	Issues      []Issue    `json:"issues"`
	Account     *Account   `json:"account"`
	DeviceName  string     `json:"device_name"`
	SignIn      *SignIn    `json:"signin"`
	Mount       Mount      `json:"mount"`
	Paused      bool       `json:"paused"`
	PausedUntil *time.Time `json:"paused_until"`
	Transfers   Summary    `json:"transfers"`
	Update      Update     `json:"update"`
	Driver      Driver     `json:"driver"`
	StartedAt   time.Time  `json:"started_at"`
	// FirstRun is true until this PC has been signed in once.
	FirstRun bool `json:"first_run"`
	// Unsent is what has not reached the drive yet (queued and held).
	Unsent Unsent `json:"unsent"`
	// Cache is how much of this PC the drive's cache uses, and for what.
	Cache CacheState `json:"cache"`
}

// OpenAction is a POST /v1/open body.
type OpenAction struct {
	Target string `json:"target"`
	Path   string `json:"path,omitempty"`
}

// NotificationAction is a notification's button.
type NotificationAction struct {
	Label string     `json:"label"`
	Open  OpenAction `json:"open"`
}

// Notification is something worth an OS toast.
type Notification struct {
	ID     string              `json:"id"`
	Kind   string              `json:"kind"`
	Title  string              `json:"title"`
	Body   string              `json:"body"`
	At     time.Time           `json:"at"`
	Action *NotificationAction `json:"action"`
}

// HeldChange is the held event.
type HeldChange struct {
	Op   string   `json:"op"` // added | removed
	Item Transfer `json:"item"`
}

// Snapshot is the first event of every stream.
type Snapshot struct {
	State  State      `json:"state"`
	Active []Transfer `json:"active"`
	Held   []Transfer `json:"held"`
	Lagged bool       `json:"lagged"`
}

// SettingsResponse is GET/PATCH /v1/settings.
type SettingsResponse struct {
	Settings       settings.Settings `json:"settings"`
	Defaults       settings.Settings `json:"defaults"`
	Choices        SettingsChoices   `json:"choices"`
	PendingRestart []string          `json:"pending_restart"`
}

// SettingsChoices lists valid values for the UI's pickers.
type SettingsChoices struct {
	// DriveLetters are the letters the drive can use: the free ones (no
	// volume, network mapping — connected or remembered — or subst), plus
	// the current one while the drive is mounted there. Last first.
	DriveLetters []string `json:"drive_letters"`
	// LettersInUse maps each other letter D:-Z: to what holds it:
	// "volume", "network" or "subst".
	LettersInUse map[string]string `json:"letters_in_use"`
	// SuggestedLetter is the configured letter if usable, otherwise the next
	// free one; "" off Windows or when every letter is taken.
	SuggestedLetter string   `json:"suggested_letter"`
	UpdateChannels  []string `json:"update_channels"`
}

// Unsent counts local changes not yet on the drive.
type Unsent struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	Held  int   `json:"held"`
}

// SignOutResult is POST /v1/signout's answer.
type SignOutResult struct {
	SignedOut    bool    `json:"signed_out"`
	Unlinked     bool    `json:"unlinked"`
	UnlinkError  *string `json:"unlink_error,omitempty"`
	CacheCleared bool    `json:"cache_cleared"`
	UnsentKept   *Unsent `json:"unsent_kept"`
}

// SendResult is POST /v1/send's answer.
type SendResult struct {
	BatchID string `json:"batch_id"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
}

// ErrorBody is every error response: the drive server's shape.
type ErrorBody struct {
	Error   string         `json:"error"`
	Message string         `json:"message"`
	Extra   map[string]any `json:"-"`
}
