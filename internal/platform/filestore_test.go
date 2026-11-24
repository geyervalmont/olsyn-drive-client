package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileCredentialStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	s, err := NewFileCredentialStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const target = "Olsyn Drive/drive.example.test"
	if _, err := s.Get(target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store Get = %v", err)
	}
	if err := s.CheckWritable(); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(target, []byte("odt_synthetic")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(target)
	if err != nil || string(got) != "odt_synthetic" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("files left: %v", entries)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{dir, filepath.Join(dir, entries[0].Name())} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s is %v, want owner-only", p, fi.Mode().Perm())
			}
		}
	}
	if err := s.Delete(target); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(target); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	if _, err := s.Get(target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete Get = %v", err)
	}
}
