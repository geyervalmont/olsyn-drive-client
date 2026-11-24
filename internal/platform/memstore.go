package platform

import "sync"

// MemoryCredentialStore is an in-memory CredentialStore for tests and for
// platforms without a real store yet. Secrets are lost when the process exits.
type MemoryCredentialStore struct {
	mu      sync.Mutex
	secrets map[string][]byte
}

// NewMemoryCredentialStore returns an empty store.
func NewMemoryCredentialStore() *MemoryCredentialStore {
	return &MemoryCredentialStore{secrets: map[string][]byte{}}
}

// Get returns a copy of the secret, or ErrNotFound.
func (m *MemoryCredentialStore) Get(target string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[target]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), s...), nil
}

// Set stores a copy of the secret.
func (m *MemoryCredentialStore) Set(target string, secret []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[target] = append([]byte(nil), secret...)
	return nil
}

// Delete removes the secret; a missing one is not an error.
func (m *MemoryCredentialStore) Delete(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.secrets, target)
	return nil
}
