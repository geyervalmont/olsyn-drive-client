package agentconn

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDialer returns the dialer for this user's agent (docs/ipc.md: Linux).
func DefaultDialer() (Dialer, error) {
	return UnixDialer{Path: linuxSocketPath(os.Getenv("XDG_RUNTIME_DIR"), os.UserHomeDir)}, nil
}

func linuxSocketPath(runtimeDir string, home func() (string, error)) string {
	if runtimeDir != "" {
		return filepath.Join(runtimeDir, "olsyn-drive", "agent.sock")
	}
	h, err := home()
	if err != nil {
		// Without a home folder there is nowhere sensible; the dial fails with this path in the error.
		return filepath.Join(os.TempDir(), fmt.Sprintf("olsyn-drive-%d", os.Getuid()), "agent.sock")
	}
	return filepath.Join(h, ".local", "state", "olsyn-drive", "run", "agent.sock")
}
