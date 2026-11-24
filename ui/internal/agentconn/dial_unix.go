//go:build !windows

package agentconn

import (
	"context"
	"fmt"
	"net"
)

// UnixDialer connects to the agent's unix socket (macOS and Linux).
type UnixDialer struct {
	Path string
}

// DialContext opens the socket.
func (d UnixDialer) DialContext(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "unix", d.Path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("dial agent socket %s: %w", d.Path, err)
		}
		return nil, &DialError{Endpoint: d.Path, Err: err}
	}
	return conn, nil
}

// Endpoint returns the socket path.
func (d UnixDialer) Endpoint() string { return d.Path }
