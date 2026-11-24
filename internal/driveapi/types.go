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

// GeneratedInfo is what a listing says about a generated entry: a file or
// folder the drive computes from other files (previews, converted textures).
// Generated files have no versions.
type GeneratedInfo struct {
	// ResultKey names the built bytes; it changes when they do. "" until built.
	ResultKey string `json:"result_key"`
	Built     bool   `json:"built"`
	// SizeExact is false while the listed size is an estimate (not built yet).
	SizeExact bool   `json:"size_exact"`
	State     string `json:"state"`
}

// GeneratedInfo returns the generated details, and whether n is generated.
func (n *Node) GeneratedInfo() (GeneratedInfo, bool) {
	s := strings.TrimSpace(string(n.Generated))
	if s == "" || s == "null" || s == "false" {
		return GeneratedInfo{}, false
	}
	var g GeneratedInfo
	_ = json.Unmarshal(n.Generated, &g) // "true" alone: generated, nothing known
	return g, true
}

// Content identities of generated files, used where a version id is used for
// stored files. GeneratedUnbuilt: the bytes and the real size are not known
// yet. GeneratedPrefix+"k:"+result key: built, as a listing named it.
// GeneratedPrefix+"e:"+ETag: as a read answered it (pinned with If-Range).
const (
	GeneratedPrefix  = "gen:"
	GeneratedUnbuilt = GeneratedPrefix
	generatedByKey   = GeneratedPrefix + "k:"
	generatedByETag  = GeneratedPrefix + "e:"
)

// IsGeneratedVersion reports a content identity of a generated file.
func IsGeneratedVersion(v string) bool { return strings.HasPrefix(v, GeneratedPrefix) }

// GeneratedETagVersion is the content identity for bytes a read answered
// with this ETag.
func GeneratedETagVersion(etag string) string { return generatedByETag + etag }

// IsGeneratedETagVersion reports an identity learnt from a read.
func IsGeneratedETagVersion(v string) bool { return strings.HasPrefix(v, generatedByETag) }

// ContentVersion identifies the bytes of a file: its current version id, or
// for a generated file one of the generated identities. "" for a folder or a
// file without content.
func (n *Node) ContentVersion() string {
	if n.Kind != KindFile {
		return ""
	}
	if g, ok := n.GeneratedInfo(); ok {
		if g.Built && g.SizeExact && g.ResultKey != "" {
			return generatedByKey + g.ResultKey
		}
		return GeneratedUnbuilt
	}
	return n.VersionID()
}

// IsManaged reports a file managed by an integration.
func (n *Node) IsManaged() bool {
	s := strings.TrimSpace(string(n.Managed))
	return s != "" && s != "null" && s != "false"
}

// Checksum states of a version (absent when no SHA-256 was claimed).
const (
	ChecksumVerified = "verified" // the drive hashed the bytes and they match
	ChecksumClaimed  = "claimed"  // a large file: the drive's own pass is still to come
	ChecksumMismatch = "mismatch" // the drive's pass found other bytes: upload again
)

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
