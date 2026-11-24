// Package core is the file system core behind the agent: it implements
// agent.FileSystem over vfs, index, cache, journal and the transfer engine,
// and agent.Drive over driveapi. One FS lives as long as the agent; each
// sign-in starts a session on it (Start) with its own index, journal and
// cache under per-account folders, so a second account on the same PC never
// sees the first one's unsent files.
package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// DriveOptions configure the drive API adapter.
type DriveOptions struct {
	HTTPClient *http.Client
	UserAgent  string
	Logger     *slog.Logger
}

// NewDrive returns the agent's view of the drive at serverURL. A bad URL
// gives a Drive whose every call fails with that error.
func NewDrive(serverURL string, o DriveOptions) agent.Drive {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	api, err := newAPI(serverURL, "", o.HTTPClient, o.UserAgent, o.Logger)
	if err != nil {
		return failedDrive{err}
	}
	return &drive{api: api, server: serverURL, opts: o}
}

func newAPI(serverURL, token string, hc *http.Client, ua string, log *slog.Logger) (*driveapi.Client, error) {
	api, err := driveapi.New(driveapi.Options{BaseURL: serverURL, HTTPClient: hc, UserAgent: ua, Logger: log,
		Token: func(context.Context) (string, error) {
			if token == "" {
				return "", errors.New("not signed in")
			}
			return token, nil
		}})
	if err != nil {
		return nil, fmt.Errorf("drive client for %s: %w", serverURL, err)
	}
	return api, nil
}

type drive struct {
	api    *driveapi.Client
	server string
	opts   DriveOptions
}

func (d *drive) withToken(token string) (*driveapi.Client, error) {
	return newAPI(d.server, token, d.opts.HTTPClient, d.opts.UserAgent, d.opts.Logger)
}

func (d *drive) LinkStart(ctx context.Context, req agent.LinkRequest) (agent.Link, error) {
	res, err := d.api.StartLink(ctx, driveapi.LinkRequest{Client: req.Client, Machine: req.Machine, AppVersion: req.AppVersion, Platform: req.Platform})
	if err != nil {
		return agent.Link{}, mapDriveErr(err)
	}
	return agent.Link{DeviceCode: res.DeviceCode, UserCode: res.UserCode, VerificationURL: res.VerificationURL,
		Interval: time.Duration(res.Interval) * time.Second, ExpiresIn: time.Duration(res.ExpiresIn) * time.Second}, nil
}

func (d *drive) LinkPoll(ctx context.Context, deviceCode string) (agent.LinkResult, error) {
	res, err := d.api.PollLink(ctx, deviceCode)
	switch {
	case err == nil:
	case driveapi.IsLinkPending(err):
		return agent.LinkResult{}, agent.ErrAuthorizationPending
	case errors.Is(err, driveapi.ErrLinkDenied):
		return agent.LinkResult{}, agent.ErrLinkDenied
	case errors.Is(err, driveapi.ErrLinkExpired):
		return agent.LinkResult{}, agent.ErrLinkExpired
	default:
		return agent.LinkResult{}, mapDriveErr(err)
	}
	out := agent.LinkResult{Token: res.Token, DeviceID: res.DeviceID,
		User: agent.User{ID: res.User.ID, Name: res.User.Name, Email: res.User.Email}}
	// whether the device was linked read-only is in the bootstrap
	if api, err := d.withToken(res.Token); err == nil {
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if b, err := api.Bootstrap(bctx); err == nil {
			out.ReadOnly = b.ReadOnly
			if out.User.Email == "" {
				out.User.Email = b.User.Email
			}
		}
		cancel()
	}
	return out, nil
}

func (d *drive) Heartbeat(ctx context.Context, token string, hb agent.Heartbeat) error {
	api, err := d.withToken(token)
	if err != nil {
		return err
	}
	err = api.SendHeartbeat(ctx, driveapi.Heartbeat{State: hb.State, MountPath: hb.MountPath, ErrorCode: hb.ErrorCode,
		AppVersion: hb.AppVersion, Platform: hb.Platform})
	return mapDriveErr(err)
}

func (d *drive) Unlink(ctx context.Context, token, deviceID string) error {
	api, err := d.withToken(token)
	if err != nil {
		return err
	}
	err = api.UnlinkDevice(ctx, deviceID)
	if driveapi.IsNotFound(err) {
		return nil // already unlinked
	}
	return mapDriveErr(err)
}

// mapDriveErr turns driveapi errors into the agent's sentinels.
func mapDriveErr(err error) error {
	switch {
	case err == nil:
		return nil
	case driveapi.IsAuthLost(err):
		return fmt.Errorf("%w: %v", agent.ErrUnauthenticated, err)
	case driveapi.IsWait(err):
		code := "server_restarting"
		if e, ok := driveapi.AsError(err); ok && e.Code != "" {
			code = e.Code
		}
		return &agent.RetryAfterError{Code: code, After: max(driveapi.RetryAfter(err), time.Second)}
	}
	return err
}

// RefusedDrive is a Drive whose every call fails with err (a server this
// build may not use, for one).
func RefusedDrive(err error) agent.Drive { return failedDrive{err} }

type failedDrive struct{ err error }

func (f failedDrive) LinkStart(context.Context, agent.LinkRequest) (agent.Link, error) {
	return agent.Link{}, f.err
}
func (f failedDrive) LinkPoll(context.Context, string) (agent.LinkResult, error) {
	return agent.LinkResult{}, f.err
}
func (f failedDrive) Heartbeat(context.Context, string, agent.Heartbeat) error { return f.err }
func (f failedDrive) Unlink(context.Context, string, string) error             { return f.err }
