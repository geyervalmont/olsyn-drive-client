// Package state keeps the client's crash-safe records: one small JSON file
// per transfer, written to a temp file, synced and renamed into place, so
// a crash leaves either the old record or the new one, never half of one.
// Records hold ids, sizes, times and hashes; never file contents.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// States a transfer moves through.
const (
	Settling   = "settling"   // spool: size/mtime recorded, waiting for them to stay put
	Ready      = "ready"      // complete on disk, waiting its turn
	Starting   = "starting"   // checked; start (or resume) asked for
	Sending    = "sending"    // upload_id known; parts going to storage
	Completing = "completing" // all parts sent; complete asked for
	Verifying  = "verifying"  // published; waiting for the drive's own hash
	Done       = "done"       // verified (or already there and identical)
	Held       = "held"       // needs a person: conflict, refusal, mismatch
	Failed     = "failed"     // a final error for this version of the file
)

// Record is one local file's transfer.
type Record struct {
	Key         string    `json:"key"`
	Rel         string    `json:"rel"` // path below its root, "/" separated
	Size        int64     `json:"size"`
	MtimeNs     int64     `json:"mtime_ns"`
	State       string    `json:"state"`
	Since       time.Time `json:"since"` // when Size/MtimeNs were first seen as they are
	Fingerprint string    `json:"fingerprint,omitempty"`
	ParentID    string    `json:"parent_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	Person      string    `json:"person,omitempty"`
	Assurance   string    `json:"assurance,omitempty"`
	UploadID    string    `json:"upload_id,omitempty"`
	VersionID   string    `json:"version_id,omitempty"`
	NodeID      string    `json:"node_id,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Outcome     string    `json:"outcome,omitempty"` // sent | already
	Reason      string    `json:"reason,omitempty"`
	Attempts    int       `json:"attempts,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store is a directory of records.
type Store struct{ dir string }

// Open makes (if needed) and opens a state directory, private to the user.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

// KeyFor names a record by what identifies a transfer: its root and path.
func KeyFor(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func (s *Store) path(key string) string { return filepath.Join(s.dir, key+".json") }

// Get loads a record; nil when there is none.
func (s *Store) Get(key string) (*Record, error) {
	b, err := os.ReadFile(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		// A torn write can't happen (rename), but a bad disk can: start the
		// file over; the drive's fingerprint resume and check stop duplicates.
		return nil, nil
	}
	return &r, nil
}

// Put writes a record durably before the step it enables.
func (s *Store) Put(r *Record) error {
	if r.Key == "" {
		return fmt.Errorf("record without key")
	}
	r.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(r.Key)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	syncDir(s.dir)
	return nil
}

// Delete drops a record.
func (s *Store) Delete(key string) error {
	err := os.Remove(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// All loads every record (startup, status).
func (s *Store) All() ([]*Record, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".tmp-") {
			_ = os.Remove(filepath.Join(s.dir, n)) // a crash between create and rename
			continue
		}
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		r, err := s.Get(strings.TrimSuffix(n, ".json"))
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}
