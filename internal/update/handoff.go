package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultHealthTimeout is how long a new version has to prove itself.
const DefaultHealthTimeout = 60 * time.Second

// Process is a started agent that can be stopped.
type Process interface {
	Kill() error
}

// Handoff runs the restart after Install. The old agent has stopped serving
// (and unmounted) before Run is called. Health contract: the new agent must
// answer IPC GET /v1/state with the new version, ready=true and, when the
// drive was mounted before, mount.state=mounted, within Timeout.
type Handoff struct {
	Start   func() (Process, error)         // starts the new agent
	Healthy func(ctx context.Context) error // one health probe; nil = healthy
	Restart func() error                    // starts the old agent after a rollback
	Store   *Store
	Version string // the new version
	Timeout time.Duration
	Every   time.Duration
	Logger  *slog.Logger
}

// Run starts the new version and waits for it to be healthy. If it is not,
// it is stopped, the old executable is restored and started again, the
// failure is recorded, and ErrRolledBack is returned.
func (h Handoff) Run(ctx context.Context, swap *Swap) error {
	log := h.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultHealthTimeout
	}
	every := h.Every
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	proc, err := h.Start()
	if err != nil {
		return h.rollback(log, swap, nil, fmt.Sprintf("start new version: %v", err))
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(every)
	defer t.Stop()
	var last error
	for {
		if err := h.Healthy(waitCtx); err == nil {
			log.Info("update healthy", "version", h.Version)
			return nil
		} else {
			last = err
		}
		select {
		case <-waitCtx.Done():
			reason := "no healthy answer within " + timeout.String()
			if last != nil {
				reason += ": " + last.Error()
			}
			return h.rollback(log, swap, proc, reason)
		case <-t.C:
		}
	}
}

func (h Handoff) rollback(log *slog.Logger, swap *Swap, proc Process, reason string) error {
	log.Warn("update failed its health check; rolling back", "version", h.Version, "reason", reason)
	var errs []error
	if proc != nil {
		if err := proc.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("stop new version: %w", err))
		}
	}
	if err := swap.Rollback(); err != nil {
		errs = append(errs, err)
	}
	if h.Store != nil {
		if err := h.Store.RecordFailure(h.Version, reason); err != nil {
			errs = append(errs, err)
		}
	}
	if h.Restart != nil {
		if err := h.Restart(); err != nil {
			errs = append(errs, fmt.Errorf("restart previous version: %w", err))
		}
	}
	return errors.Join(append([]error{fmt.Errorf("%s: %w", reason, ErrRolledBack)}, errs...)...)
}
