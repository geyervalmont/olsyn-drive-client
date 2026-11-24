package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileCredentialStore keeps each secret in its own file in one folder that
// only this user can read (0700/0600 on Unix, an owner-only ACL on Windows).
// It exists for development builds driven over SSH, where the OS store is
// not available; release builds never use it.
type FileCredentialStore struct {
	dir string
}

// NewFileCredentialStore returns a store in dir, creating dir owner-only.
func NewFileCredentialStore(dir string) (*FileCredentialStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create credential folder: %w", err)
	}
	if err := restrictToOwner(dir, true); err != nil {
		return nil, fmt.Errorf("restrict credential folder: %w", err)
	}
	return &FileCredentialStore{dir: dir}, nil
}

// file names a target by its hash: targets hold a host name and a slash.
func (s *FileCredentialStore) file(target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:16])+".secret")
}

// Get returns the secret, or ErrNotFound.
func (s *FileCredentialStore) Get(target string) ([]byte, error) {
	data, err := os.ReadFile(s.file(target))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	return data, nil
}

// Set writes the secret through a temporary file, owner-only, then renames it.
func (s *FileCredentialStore) Set(target string, secret []byte) error {
	path := s.file(target)
	tmp, err := os.CreateTemp(s.dir, ".secret-*")
	if err != nil {
		return fmt.Errorf("write credential file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := restrictToOwner(name, false); err != nil {
		tmp.Close()
		return fmt.Errorf("restrict credential file: %w", err)
	}
	if _, err := tmp.Write(secret); err != nil {
		tmp.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("save credential file: %w", err)
	}
	return nil
}

// Delete removes the secret; a missing one is not an error.
func (s *FileCredentialStore) Delete(target string) error {
	err := os.Remove(s.file(target))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete credential file: %w", err)
	}
	return nil
}

// CheckWritable reports whether the folder takes a file.
func (s *FileCredentialStore) CheckWritable() error {
	const probe = "write-check"
	if err := s.Set(probe, []byte{1}); err != nil {
		return err
	}
	return s.Delete(probe)
}
