package fakedrive

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestTreeListsFoldersThenContents(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	a := s.AddFolder(s.RootID(), "A")
	s.AddFile(a, "one.txt", []byte("one"))
	b := s.AddFolder(a, "B")
	s.AddFile(b, "two.txt", []byte("two!"))
	got, ok := s.Tree("/A")
	if !ok || len(got) != 3 {
		t.Fatalf("tree = %+v, %v", got, ok)
	}
	sum := sha256.Sum256([]byte("two!"))
	want := map[string]TreeEntry{
		"one.txt":   {Kind: "file", Size: 3, Versions: 1},
		"B":         {Kind: "folder"},
		"B/two.txt": {Kind: "file", Size: 4, Versions: 1, SHA256: hex.EncodeToString(sum[:])},
	}
	for _, e := range got {
		w, found := want[e.Path]
		if !found || e.Kind != w.Kind || e.Size != w.Size || e.Versions != w.Versions || (w.SHA256 != "" && e.SHA256 != w.SHA256) || e.ID == "" {
			t.Errorf("entry %+v, want %+v", e, w)
		}
	}
	if _, ok := s.Tree("/A/one.txt"); ok {
		t.Fatal("a file is not a folder")
	}
}
