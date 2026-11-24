//go:build !windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Endpoint is the agent's unix socket in the runtime folder.
func Endpoint(p Paths) (string, error) {
	if p.Runtime == "" {
		return "", errors.New("no runtime folder for the agent socket")
	}
	ep := filepath.Join(p.Runtime, "agent.sock")
	// sockaddr_un holds 104 bytes on macOS (108 on Linux), with the final NUL.
	if len(ep) > 103 {
		return "", fmt.Errorf("the agent's socket path %s is %d characters, over the system's limit of 103: use a shorter OLSYN_DRIVE_HOME", ep, len(ep))
	}
	return ep, nil
}

// Listen creates the socket with mode 0600 inside a 0700 folder owned by this
// user. A live agent on the socket gives ErrAlreadyRunning; a stale socket
// file left by a crash is removed.
func Listen(endpoint string) (net.Listener, error) {
	dir := filepath.Dir(endpoint)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict %s: %w", dir, err)
	}
	if err := checkOwner(dir); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(endpoint); err == nil {
		c, derr := net.DialTimeout("unix", endpoint, time.Second)
		if derr == nil {
			c.Close()
			return nil, fmt.Errorf("listen on %s: %w", endpoint, ErrAlreadyRunning)
		}
		if err := os.Remove(endpoint); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", endpoint, err)
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restrict socket: %w", err)
	}
	return l, nil
}

// Dial connects to the agent's socket.
func Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	return c, nil
}

func checkOwner(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user; refusing to put the agent socket there", dir)
	}
	return nil
}
