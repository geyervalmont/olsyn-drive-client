package index

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPutLookupListDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db := open(t, path)
	err := db.Update(func(tx *Tx) error {
		if err := tx.Put(&Entry{ID: "root", ServerID: "root", Kind: KindFolder}); err != nil {
			return err
		}
		if err := tx.Put(&Entry{ID: "d1", ServerID: "s-d1", ParentID: "root", Name: "Docs", Kind: KindFolder}); err != nil {
			return err
		}
		for i := 0; i < 25; i++ {
			if err := tx.Put(&Entry{ID: fmt.Sprintf("f%02d", i), ServerID: fmt.Sprintf("s-f%02d", i), ParentID: "d1", Name: fmt.Sprintf("File %02d.txt", i), Kind: KindFile, Size: int64(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := db.Lookup("d1", "file 07.TXT")
	if err != nil || e.ID != "f07" {
		t.Fatalf("lookup %v %v", e, err)
	}
	if err := db.Put(&Entry{ID: "x", ParentID: "d1", Name: "FILE 07.txt", Kind: KindFile}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	// page through with markers
	var names []string
	marker := ""
	for {
		page, next, err := db.List("d1", marker, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			names = append(names, p.Name)
		}
		if next == "" {
			break
		}
		marker = next
	}
	if len(names) != 25 || names[0] != "File 00.txt" || names[24] != "File 24.txt" {
		t.Fatalf("names %v", names)
	}
	// rename keeps the name index in step
	e.Name = "renamed.txt"
	if err := db.Put(e); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Lookup("d1", "File 07.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old name still found")
	}
	if got, err := db.BySID("s-f07"); err != nil || got.Name != "renamed.txt" {
		t.Fatalf("by sid %v %v", got, err)
	}
	// persisted across reopen
	if err := db.SetMeta("cursor", "42"); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error { return tx.SetListed("d1", true) }); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = open(t, path)
	defer db.Close()
	if db.Meta("cursor") != "42" || !db.Listed("d1") {
		t.Fatal("meta or listing lost on reopen")
	}
	var removed []*Entry
	if err := db.Update(func(tx *Tx) (err error) { removed, err = tx.Delete("d1"); return err }); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 26 || db.Listed("d1") {
		t.Fatalf("removed %d", len(removed))
	}
	if _, err := db.BySID("s-f03"); !errors.Is(err, ErrNotFound) {
		t.Fatal("subtree not removed")
	}
	if err := db.Update(func(tx *Tx) error { return tx.ForgetListings() }); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedFileIsSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("a damaged index should be replaced: %v", err)
	}
	defer d.Close()
	if _, err := os.Stat(path + ".damaged"); err != nil {
		t.Fatal("damaged file not kept aside")
	}
	if err := d.Put(&Entry{ID: "root", Kind: KindFolder}); err != nil {
		t.Fatal(err)
	}
}
