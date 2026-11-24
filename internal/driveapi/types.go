package driveapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Node kinds.
const (
	KindFolder   = "folder"
	KindFile     = "file"
	KindShortcut = "shortcut"
)

// Node is a file, folder or shortcut as the drive's listings return it.
// Only the fields the desktop client uses are typed; the rest are ignored.
type Node struct {
	ID             string          `json:"id"`
	ParentID       string          `json:"parent_id,omitempty"`
	Kind           string          `json:"kind"`
	Name           string          `json:"name"`
	Size           int64           `json:"size"`
	Mime           string          `json:"mime,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	ModifiedAt     *Time           `json:"modified_at,omitempty"`
	CurrentVersion *Version        `json:"current_version,omitempty"`
	Role           string          `json:"role,omitempty"`
	Can            Can             `json:"can"`
	URI            string          `json:"uri,omitempty"`
	TrashedAt      *time.Time      `json:"trashed_at,omitempty"`
	Sealed         bool            `json:"sealed,omitempty"`
	Managed        json.RawMessage `json:"managed,omitempty"`
	Target         json.RawMessage `json:"target,omitempty"`
	Archive        json.RawMessage `json:"archive,omitempty"`
	Generated      json.RawMessage `json:"generated,omitempty"`
}

// IsDir reports a folder.
func (n *Node) IsDir() bool { return n.Kind == KindFolder }

// VersionID returns the current version id, or "".
func (n *Node) VersionID() string {
	if n.CurrentVersion == nil {
		return ""
	}
	return n.CurrentVersion.ID
}

// MTime is the time to show as "date modified": the client modified time
// when one was set, else updated_at.
func (n *Node) MTime() time.Time {
	if n.ModifiedAt != nil && !n.ModifiedAt.IsZero() {
		return n.ModifiedAt.Time
	}
	return n.UpdatedAt
}

// IsManaged reports a file managed by an integration.
func (n *Node) IsManaged() bool {
	s := strings.TrimSpace(string(n.Managed))
	return s != "" && s != "null" && s != "false"
}

// Version is a file version.
type Version struct {
	ID            string    `json:"id"`
	Number        int       `json:"number"`
	Size          int64     `json:"size"`
	Mime          string    `json:"mime,omitempty"`
	SHA256        string    `json:"sha256,omitempty"`
	ChecksumState string    `json:"checksum_state,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ModifiedAt    *Time     `json:"modified_at,omitempty"`
}

// Can is the caller's rights on a node.
type Can struct {
	Read   bool `json:"read"`
	Upload bool `json:"upload"`
	Write  bool `json:"write"`
	Delete bool `json:"delete"`
	Share  bool `json:"share"`
	Manage bool `json:"manage"`
}

// User is the signed-in person.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// modifiedAtLayout is the drive's one wire format for modified_at:
// RFC 3339 UTC with exactly three fractional digits.
const modifiedAtLayout = "2006-01-02T15:04:05.000Z"

// Time is a client modified time with millisecond precision, sent and
// read in the drive's modified_at format.
type Time struct{ time.Time }

// NewTime truncates t to the millisecond, in UTC.
func NewTime(t time.Time) *Time {
	return &Time{t.UTC().Truncate(time.Millisecond)}
}

// FormatModifiedAt formats t in the modified_at wire format.
func FormatModifiedAt(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(modifiedAtLayout)
}

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(FormatModifiedAt(t.Time))
}

// UnmarshalJSON implements json.Unmarshaler. It accepts any RFC 3339 instant.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("modified_at: %w", err)
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("modified_at: %w", err)
	}
	t.Time = v.UTC()
	return nil
}

// Page is one page of a paged list.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor"`
}
