// Package index is the local metadata database of the remote tree: what
// the drive holds, as far as this PC has listed it, kept fresh by the
// changes feed, plus the local names and sizes of changes not yet sent.
//
// It is one bbolt file. Every entry has a stable local id: a node that came
// from the drive keeps the drive's id; a node made on this PC gets a "l-" id
// and learns its drive id (ServerID) once created there. Children are keyed
// by (parent id, folded name), so lookups are case-insensitive like the
// drive and listings come back in name order with a resumable marker.
package index

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Kinds.
const (
	KindFolder = "folder"
	KindFile   = "file"
)

// Entry is one file or folder.
type Entry struct {
	ID       string    `json:"id"`
	ServerID string    `json:"sid,omitempty"` // "" until the drive has it
	ParentID string    `json:"pid"`           // local id of the parent ("" for the root)
	Name     string    `json:"name"`          // the drive's name (not mapped for the OS)
	Kind     string    `json:"kind"`
	Size     int64     `json:"size"`
	MTime    time.Time `json:"mtime"`
	Created  time.Time `json:"ctime"`
	// VersionID is the drive version the content is based on ("" for a new
	// local file or a folder).
	VersionID string `json:"ver,omitempty"`
	SHA256    string `json:"sha,omitempty"`
	// ContentKey is the cache key of local content not yet on the drive ("" when
	// the content is the drive's VersionID).
	ContentKey string `json:"ckey,omitempty"`
	// Rights and states from the drive.
	CanWrite  bool `json:"w,omitempty"`
	CanUpload bool `json:"u,omitempty"`
	CanDelete bool `json:"d,omitempty"`
	Sealed    bool `json:"sealed,omitempty"`
	Managed   bool `json:"managed,omitempty"`
	// Attributes kept for the OS only (hidden, read-only, …), never sent.
	Attrs uint32 `json:"attrs,omitempty"`
	// LocalOnly marks junk the OS made (Thumbs.db, ~$ lock files, …): it
	// stays on this PC and is never sent.
	LocalOnly bool `json:"local_only,omitempty"`
}

// IsDir reports a folder.
func (e *Entry) IsDir() bool { return e.Kind == KindFolder }

// Local reports an entry made on this PC that the drive does not have yet.
func (e *Entry) Local() bool { return e.ServerID == "" }

var (
	bNodes = []byte("nodes")
	bKids  = []byte("kids")
	bSIDs  = []byte("sids")
	bDirs  = []byte("dirs")
	bMeta  = []byte("meta")
)

// ErrNotFound is returned for unknown ids and names.
var ErrNotFound = errors.New("index: not found")

// ErrNameTaken is returned when a Put would give two entries one name.
var ErrNameTaken = errors.New("index: name already used")

// DB is the index.
type DB struct {
	db *bolt.DB
}

// Open opens (or creates) the index at path.
//
// The index is written without fsync: it is a copy of what the drive says
// plus local entries the journal can rebuild, so a file the OS lost on a
// power cut is checked at the next open and, if damaged, set aside and
// started again (the caller then lists from the drive and replays the
// journal). A process crash loses nothing: the writes are in the OS cache.
func Open(path string) (*DB, error) {
	d, err := openDB(path)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, errDamaged) {
		return nil, err
	}
	if rerr := os.Rename(path, path+".damaged"); rerr != nil {
		return nil, fmt.Errorf("index: set aside damaged %s: %w", path, rerr)
	}
	return openDB(path)
}

var errDamaged = errors.New("index: damaged")

var keyOpen = []byte("\x00open")

func openDB(path string) (*DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, fmt.Errorf("index: open %s: %w", path, err)
		}
		return nil, fmt.Errorf("%w: open %s: %v", errDamaged, path, err)
	}
	unclean := false
	_ = db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bMeta); b != nil && b.Get(keyOpen) != nil {
			unclean = true
		}
		return nil
	})
	if unclean {
		var bad error
		_ = db.View(func(tx *bolt.Tx) error {
			for err := range tx.Check() {
				if bad == nil {
					bad = err
				}
			}
			return nil
		})
		if bad != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%w: %s: %v", errDamaged, path, bad)
		}
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bNodes, bKids, bSIDs, bDirs, bMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return tx.Bucket(bMeta).Put(keyOpen, []byte{1})
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index: init %s: %w", path, err)
	}
	db.NoSync = true
	return &DB{db: db}, nil
}

// Close marks the file cleanly closed, syncs it and closes it.
func (d *DB) Close() error {
	err := d.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bMeta).Delete(keyOpen) })
	if err == nil {
		err = d.db.Sync()
	}
	if cerr := d.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// Fold is the case-insensitive form of a name used for lookups.
func Fold(name string) string { return strings.ToLower(name) }

func kidKey(parent, name string) []byte {
	k := make([]byte, 0, len(parent)+1+len(name))
	k = append(k, parent...)
	k = append(k, 0)
	k = append(k, Fold(name)...)
	return k
}

// Tx is a read-write transaction over the index.
type Tx struct{ tx *bolt.Tx }

// Update runs fn in one atomic, durable transaction.
func (d *DB) Update(fn func(tx *Tx) error) error {
	return d.db.Update(func(btx *bolt.Tx) error { return fn(&Tx{btx}) })
}

// View runs fn in a read-only transaction.
func (d *DB) View(fn func(tx *Tx) error) error {
	return d.db.View(func(btx *bolt.Tx) error { return fn(&Tx{btx}) })
}

// Get reads an entry by local id.
func (t *Tx) Get(id string) (*Entry, error) {
	v := t.tx.Bucket(bNodes).Get([]byte(id))
	if v == nil {
		return nil, ErrNotFound
	}
	var e Entry
	if err := json.Unmarshal(v, &e); err != nil {
		return nil, fmt.Errorf("index: entry %s: %w", id, err)
	}
	return &e, nil
}

// BySID finds the entry for a drive id.
func (t *Tx) BySID(sid string) (*Entry, error) {
	id := t.tx.Bucket(bSIDs).Get([]byte(sid))
	if id == nil {
		return nil, ErrNotFound
	}
	return t.Get(string(id))
}

// Lookup finds a child by name, case-insensitively.
func (t *Tx) Lookup(parentID, name string) (*Entry, error) {
	id := t.tx.Bucket(bKids).Get(kidKey(parentID, name))
	if id == nil {
		return nil, ErrNotFound
	}
	return t.Get(string(id))
}

// Put inserts or replaces an entry, keeping the name and drive-id indexes in
// step. A name already used by another entry in the same folder is an error.
func (t *Tx) Put(e *Entry) error {
	nodes, kids, sids := t.tx.Bucket(bNodes), t.tx.Bucket(bKids), t.tx.Bucket(bSIDs)
	if old, err := t.Get(e.ID); err == nil {
		if old.ParentID != "" && (old.ParentID != e.ParentID || Fold(old.Name) != Fold(e.Name)) {
			if err := kids.Delete(kidKey(old.ParentID, old.Name)); err != nil {
				return err
			}
		}
		if old.ServerID != "" && old.ServerID != e.ServerID {
			if err := sids.Delete([]byte(old.ServerID)); err != nil {
				return err
			}
		}
	}
	if e.ParentID != "" { // the root has no parent and no name entry
		k := kidKey(e.ParentID, e.Name)
		if cur := kids.Get(k); cur != nil && string(cur) != e.ID {
			return fmt.Errorf("%w: %q in folder %s is %s", ErrNameTaken, e.Name, e.ParentID, cur)
		}
		if err := kids.Put(k, []byte(e.ID)); err != nil {
			return err
		}
	}
	if e.ServerID != "" {
		if err := sids.Put([]byte(e.ServerID), []byte(e.ID)); err != nil {
			return err
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return nodes.Put([]byte(e.ID), b)
}

// Delete removes an entry and, for a folder, everything below it. It
// returns the removed entries (so callers can drop cached content).
func (t *Tx) Delete(id string) ([]*Entry, error) {
	e, err := t.Get(id)
	if err != nil {
		return nil, err
	}
	var removed []*Entry
	if e.IsDir() {
		kids, err := t.Children(id)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			r, err := t.Delete(k.ID)
			if err != nil {
				return nil, err
			}
			removed = append(removed, r...)
		}
		if err := t.tx.Bucket(bDirs).Delete([]byte(id)); err != nil {
			return nil, err
		}
	}
	if err := t.tx.Bucket(bKids).Delete(kidKey(e.ParentID, e.Name)); err != nil {
		return nil, err
	}
	if e.ServerID != "" {
		if err := t.tx.Bucket(bSIDs).Delete([]byte(e.ServerID)); err != nil {
			return nil, err
		}
	}
	if err := t.tx.Bucket(bNodes).Delete([]byte(id)); err != nil {
		return nil, err
	}
	return append(removed, e), nil
}

// Children returns every child of a folder in folded-name order.
func (t *Tx) Children(parentID string) ([]*Entry, error) {
	out, _, err := t.List(parentID, "", 0)
	return out, err
}

// List returns up to limit children (0 = all) whose folded name sorts after
// marker ("" = from the start), and the marker for the next page ("" when
// done).
func (t *Tx) List(parentID, marker string, limit int) ([]*Entry, string, error) {
	prefix := append([]byte(parentID), 0)
	c := t.tx.Bucket(bKids).Cursor()
	var k, v []byte
	if marker == "" {
		k, v = c.Seek(prefix)
	} else {
		mk := kidKey(parentID, marker)
		k, v = c.Seek(mk)
		if k != nil && bytes.Equal(k, mk) {
			k, v = c.Next()
		}
	}
	var out []*Entry
	for ; k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		if limit > 0 && len(out) == limit {
			return out, out[len(out)-1].Name, nil
		}
		e, err := t.Get(string(v))
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
	}
	return out, "", nil
}

// Listed reports whether a folder's children were fetched from the drive.
func (t *Tx) Listed(folderID string) bool {
	return t.tx.Bucket(bDirs).Get([]byte(folderID)) != nil
}

// SetListed records that a folder's children are known (or forgets it).
func (t *Tx) SetListed(folderID string, listed bool) error {
	if !listed {
		return t.tx.Bucket(bDirs).Delete([]byte(folderID))
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(time.Now().Unix()))
	return t.tx.Bucket(bDirs).Put([]byte(folderID), b[:])
}

// ForgetListings marks every folder as not listed (after a reset).
func (t *Tx) ForgetListings() error {
	if err := t.tx.DeleteBucket(bDirs); err != nil {
		return err
	}
	_, err := t.tx.CreateBucket(bDirs)
	return err
}

// All calls fn for every entry (in id order).
func (t *Tx) All(fn func(*Entry) error) error {
	return t.tx.Bucket(bNodes).ForEach(func(k, v []byte) error {
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("index: entry %s: %w", k, err)
		}
		return fn(&e)
	})
}

// Meta reads a small value ("" when unset).
func (t *Tx) Meta(key string) string {
	return string(t.tx.Bucket(bMeta).Get([]byte(key)))
}

// SetMeta writes a small value.
func (t *Tx) SetMeta(key, value string) error {
	return t.tx.Bucket(bMeta).Put([]byte(key), []byte(value))
}

// DeleteMeta removes a metadata value.
func (t *Tx) DeleteMeta(key string) error {
	return t.tx.Bucket(bMeta).Delete([]byte(key))
}

// Convenience wrappers for single operations.

// Get reads an entry.
func (d *DB) Get(id string) (e *Entry, err error) {
	err = d.View(func(tx *Tx) error { e, err = tx.Get(id); return err })
	return e, err
}

// Lookup finds a child by name.
func (d *DB) Lookup(parentID, name string) (e *Entry, err error) {
	err = d.View(func(tx *Tx) error { e, err = tx.Lookup(parentID, name); return err })
	return e, err
}

// BySID finds the entry for a drive id.
func (d *DB) BySID(sid string) (e *Entry, err error) {
	err = d.View(func(tx *Tx) error { e, err = tx.BySID(sid); return err })
	return e, err
}

// List pages through a folder's children.
func (d *DB) List(parentID, marker string, limit int) (out []*Entry, next string, err error) {
	err = d.View(func(tx *Tx) error { out, next, err = tx.List(parentID, marker, limit); return err })
	return out, next, err
}

// Listed reports whether a folder was listed.
func (d *DB) Listed(folderID string) (ok bool) {
	_ = d.View(func(tx *Tx) error { ok = tx.Listed(folderID); return nil })
	return ok
}

// Put writes one entry.
func (d *DB) Put(e *Entry) error {
	return d.Update(func(tx *Tx) error { return tx.Put(e) })
}

// Meta reads a small value.
func (d *DB) Meta(key string) (v string) {
	_ = d.View(func(tx *Tx) error { v = tx.Meta(key); return nil })
	return v
}

// SetMeta writes a small value.
func (d *DB) SetMeta(key, value string) error {
	return d.Update(func(tx *Tx) error { return tx.SetMeta(key, value) })
}
