package driveapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// LinkRequest describes this PC to the person approving the link.
type LinkRequest struct {
	Client     string `json:"client"`      // e.g. "olsyn-drive-windows"
	Machine    string `json:"machine"`     // the PC's name, shown on the approval page
	AppVersion string `json:"app_version"` // this program's version
	Platform   string `json:"platform,omitempty"`
}

// LinkStart is the answer to starting a link. DeviceCode is a secret: never
// show or log it.
type LinkStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// LinkResult is the token the drive hands over once, after approval.
type LinkResult struct {
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
	User     User   `json:"user"`
}

// ErrLinkDenied and ErrLinkExpired end a link attempt.
var (
	ErrLinkDenied  = errors.New("drive: the link was denied in the browser")
	ErrLinkExpired = errors.New("drive: the link code expired or was already used")
)

// StartLink begins the device-code flow (no auth).
func (c *Client) StartLink(ctx context.Context, req LinkRequest) (*LinkStart, error) {
	var out LinkStart
	err := c.do(ctx, request{op: "start link", method: http.MethodPost, path: "/api/mount/v1/link", body: req, noAuth: true}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// PollLink asks once whether the link was approved. While the person has
// not answered it returns an *Error with Code authorization_pending.
func (c *Client) PollLink(ctx context.Context, deviceCode string) (*LinkResult, error) {
	var out LinkResult
	err := c.do(ctx, request{op: "poll link", method: http.MethodPost, path: "/api/mount/v1/link/token",
		body: map[string]string{"device_code": deviceCode}, noAuth: true}, &out)
	if err != nil {
		if e, ok := AsError(err); ok {
			switch e.Status {
			case http.StatusForbidden:
				return nil, fmt.Errorf("%w (%s)", ErrLinkDenied, e.Code)
			case http.StatusGone, http.StatusNotFound:
				return nil, fmt.Errorf("%w (%s)", ErrLinkExpired, e.Code)
			}
		}
		return nil, err
	}
	return &out, nil
}

// IsLinkPending reports 428 authorization_pending.
func IsLinkPending(err error) bool {
	e, ok := AsError(err)
	return ok && (e.Code == CodeAuthPending || e.Status == http.StatusPreconditionRequired)
}

// WaitLink polls until the person approves or denies, the code expires or
// ctx ends. Network blips and rate limits are waited out.
func (c *Client) WaitLink(ctx context.Context, start *LinkStart) (*LinkResult, error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	if start.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		res, err := c.PollLink(ctx, start.DeviceCode)
		switch {
		case err == nil:
			return res, nil
		case IsLinkPending(err), IsRetryable(err):
			wait := interval
			if ra := RetryAfter(err); ra > wait {
				wait = ra
			}
			if time.Now().Add(wait).After(deadline) {
				return nil, ErrLinkExpired
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
}

// Bootstrap is the mount surface's description of this device and drive.
type Bootstrap struct {
	Contract     string          `json:"contract"`
	User         User            `json:"user"`
	ReadOnly     bool            `json:"read_only"`
	WebURL       string          `json:"web_url"`
	Capabilities map[string]bool `json:"capabilities"`
	Limits       struct {
		MaxRangeBytes int64 `json:"max_range_bytes"`
		MaxFileBytes  int64 `json:"max_file_bytes"`
	} `json:"limits"`
	Refresh struct {
		ManifestSeconds      int `json:"manifest_seconds"`
		HeartbeatSeconds     int `json:"heartbeat_seconds"`
		AccessRecheckSeconds int `json:"access_recheck_seconds"`
	} `json:"refresh"`
	Endpoints map[string]string `json:"endpoints"`
}

// Bootstrap reads the mount bootstrap (mount tokens only).
func (c *Client) Bootstrap(ctx context.Context) (*Bootstrap, error) {
	var out Bootstrap
	if err := c.do(ctx, request{op: "bootstrap", method: http.MethodGet, path: "/api/mount/v1/bootstrap"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Me is the signed-in person and their drive root (empty for guests).
type Me struct {
	User   User   `json:"user"`
	RootID string `json:"root_id"`
}

// Me reads /api/v1/me.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.do(ctx, request{op: "me", method: http.MethodGet, path: "/api/v1/me"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat is what the device reports every heartbeat interval.
// ErrorCode must match [a-z0-9_.-]{0,64}; never free text.
type Heartbeat struct {
	State      string `json:"state"`
	MountPath  string `json:"mount_path"`
	ErrorCode  string `json:"error_code"`
	AppVersion string `json:"app_version"`
	Platform   string `json:"platform,omitempty"`
}

// SendHeartbeat posts a heartbeat. A 401 here means the device was unlinked.
func (c *Client) SendHeartbeat(ctx context.Context, hb Heartbeat) error {
	return c.do(ctx, request{op: "heartbeat", method: http.MethodPost, path: "/api/mount/v1/heartbeat", body: hb, noRateWait: true}, nil)
}

// UnlinkDevice revokes a linked device and its token
// (DELETE /api/v1/devices/{id}). After it the token answers 401.
func (c *Client) UnlinkDevice(ctx context.Context, deviceID string) error {
	return c.do(ctx, request{op: "unlink device", method: http.MethodDelete, path: pathID("/api/v1/devices/", deviceID, "")}, nil)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
