package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store persists what the updater has accepted: the newest published_at per
// channel (anti-replay) and versions that failed their health check.
type Store struct {
	path string
	mu   sync.Mutex
}

type storeData struct {
	Channels map[string]channelState `json:"channels"`
	Failed   map[string]string       `json:"failed"` // version -> reason
}

type channelState struct {
	NewestPublishedAt time.Time `json:"newest_published_at"`
	Version           string    `json:"version"`
}

// NewStore uses the JSON file at path (created on first write).
func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) load() (storeData, error) {
	d := storeData{Channels: map[string]channelState{}, Failed: map[string]string{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return d, fmt.Errorf("read update state: %w", err)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("parse update state: %w", err)
	}
	if d.Channels == nil {
		d.Channels = map[string]channelState{}
	}
	if d.Failed == nil {
		d.Failed = map[string]string{}
	}
	return d, nil
}

func (s *Store) save(d storeData) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create state folder: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write update state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace update state: %w", err)
	}
	return nil
}

// Accept records m as seen on its channel, or returns ErrReplayed when an
// already accepted manifest of that channel was published later.
func (s *Store) Accept(m Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	cur := d.Channels[m.Channel]
	if m.PublishedAt.Before(cur.NewestPublishedAt) {
		return fmt.Errorf("published %s, already accepted %s: %w",
			m.PublishedAt.Format(time.RFC3339), cur.NewestPublishedAt.Format(time.RFC3339), ErrReplayed)
	}
	if m.PublishedAt.Equal(cur.NewestPublishedAt) && cur.Version == m.Version {
		return nil
	}
	d.Channels[m.Channel] = channelState{NewestPublishedAt: m.PublishedAt, Version: m.Version}
	return s.save(d)
}

// RecordFailure remembers that version failed, so it is not applied again on its own.
func (s *Store) RecordFailure(version, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return err
	}
	d.Failed[version] = reason
	return s.save(d)
}

// Failed returns the recorded failure reason for version, if any.
func (s *Store) Failed(version string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load()
	if err != nil {
		return "", false
	}
	r, ok := d.Failed[version]
	return r, ok
}
