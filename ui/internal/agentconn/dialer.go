// Package agentconn reaches the Olsyn Drive agent over its per-user local
// endpoint (docs/ipc.md "Transport"): a named pipe on Windows, a unix socket
// elsewhere. It speaks plain HTTP/1.1 and Server-Sent Events over that
// connection and can start the agent when nothing answers.
//
// It deliberately does not import the root module: the UI and the agent share
// only the wire contract.
package agentconn

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Dialer opens one connection to the agent's endpoint.
type Dialer interface {
	DialContext(ctx context.Context) (net.Conn, error)
	// Endpoint names the endpoint for log lines: a pipe name or a socket path.
	Endpoint() string
}

// DialError is a failure to reach the agent's endpoint at all: nothing was
// sent, so the agent is not running (or not yet listening) and a request may
// be retried once it is, even a POST.
type DialError struct {
	Endpoint string
	Err      error
}

func (e *DialError) Error() string { return "open agent endpoint " + e.Endpoint + ": " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// NotRunning reports whether err means nothing answers on the agent's endpoint.
func NotRunning(err error) bool {
	var d *DialError
	return errors.As(err, &d)
}

// BaseURL is the URL prefix for every request; the host is what docs/ipc.md asks clients to send.
const BaseURL = "http://olsyn-drive"

// NewHTTPClient returns an HTTP client whose every connection goes through d.
// It has no overall timeout because the event stream is long-lived; callers
// put deadlines on their contexts.
func NewHTTPClient(d Dialer) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx)
		},
		DisableCompression:    true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 35 * time.Second, // POST /v1/update/check may take up to 30 s
		ForceAttemptHTTP2:     false,
	}
	return &http.Client{Transport: transport}
}
