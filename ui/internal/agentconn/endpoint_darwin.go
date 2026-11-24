package agentconn

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDialer returns the dialer for this user's agent (docs/ipc.md: macOS).
func DefaultDialer() (Dialer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home folder: %w", err)
	}
	return UnixDialer{Path: filepath.Join(home, "Library", "Application Support", "Olsyn Drive", "run", "agent.sock")}, nil
}
