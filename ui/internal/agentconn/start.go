package agentconn

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Starter launches "olsyn-drive agent" as a detached background process.
type Starter struct {
	Exe  string
	Args []string
}

// AgentExeEnv overrides where the agent binary is (for development).
const AgentExeEnv = "OLSYN_DRIVE_AGENT_EXE"

// FindAgent locates the agent binary: the override, then olsyn-drive next to
// this program, then on PATH. When the UI and the agent are one binary, the
// sibling is this program itself, which is what "olsyn-drive agent" means.
func FindAgent() (*Starter, error) {
	name := "olsyn-drive"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if p := os.Getenv(AgentExeEnv); p != "" {
		return &Starter{Exe: p, Args: []string{"agent"}}, nil
	}
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(candidate); err == nil {
			return &Starter{Exe: candidate, Args: []string{"agent"}}, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return &Starter{Exe: p, Args: []string{"agent"}}, nil
	}
	return nil, errors.New("olsyn-drive agent program not found next to the app or on PATH")
}

// String names the program for log lines.
func (s *Starter) String() string { return s.Exe }

// Start launches the agent and does not wait for it.
func (s *Starter) Start() error {
	cmd := exec.Command(s.Exe, s.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent %s: %w", s.Exe, err)
	}
	// The agent outlives the UI; do not keep a handle to it.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release agent process: %w", err)
	}
	return nil
}
