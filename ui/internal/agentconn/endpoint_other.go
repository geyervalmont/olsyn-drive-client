//go:build !windows && !darwin && !linux

package agentconn

import "errors"

// DefaultDialer reports that this OS has no agent endpoint defined in docs/ipc.md.
func DefaultDialer() (Dialer, error) {
	return nil, errors.New("olsyn drive: no agent endpoint is defined for this operating system")
}
