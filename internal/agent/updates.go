package agent

import (
	"context"
	"errors"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// updateState is the updater's private part of the agent state.
type updateState struct {
	rel     *update.Release
	staged  string
	when    string // "idle" or "now" once the person or policy asked to apply
	running bool   // a check or download is in progress
}

func driverAtLeast(installed, minVersion string) bool {
	return update.DriverAtLeast(installed, minVersion)
}

// updateLoop checks at start-up (after a delay) and then every interval, and
// applies a staged update once the drive is idle.
func (a *Agent) updateLoop(ctx context.Context) {
	first := time.NewTimer(a.opts.UpdateFirstCheck)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	a.CheckUpdate(ctx)
	check := time.NewTicker(a.opts.UpdateInterval)
	defer check.Stop()
	idle := time.NewTicker(30 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			a.CheckUpdate(ctx)
		case <-idle.C:
			a.applyIfIdle()
		}
	}
}

// CheckUpdate runs one check (and the download when there is something new).
func (a *Agent) CheckUpdate(ctx context.Context) Update {
	if a.opts.Updater == nil {
		return a.State().Update
	}
	a.mu.Lock()
	if a.upd.running {
		u := a.update
		a.mu.Unlock()
		return u
	}
	a.upd.running = true
	channel := a.settings.UpdateChannel
	a.update.State, a.update.Channel, a.update.Error = "checking", channel, nil
	a.mu.Unlock()
	a.publishUpdate()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	rel, err := a.opts.Updater.Check(cctx, channel)
	cancel()
	now := time.Now().UTC()
	a.mu.Lock()
	a.update.CheckedAt = &now
	a.upd.running = false
	switch {
	case errors.Is(err, update.ErrNoRelease):
		a.update.State = "up_to_date"
	case err != nil:
		a.update.State = "failed"
		a.update.Error = updateProblem(err)
	default:
		a.applyReleaseLocked(rel)
	}
	download := a.update.State == "available" && a.update.DriverRequired == nil && rel != nil && rel.PreviouslyFailed == ""
	a.mu.Unlock()
	if err != nil && !errors.Is(err, update.ErrNoRelease) {
		a.log.Warn("update check failed", "err", err)
	}
	a.publishUpdate()
	a.emitState()
	if download {
		a.downloadUpdate(ctx)
	}
	a.mu.Lock()
	u := a.update
	a.mu.Unlock()
	return u
}

func (a *Agent) applyReleaseLocked(rel *update.Release) {
	m := rel.Manifest
	a.update.Required = rel.Required
	a.update.DriverRequired = nil
	a.driver.MinVersion = a.opts.DriverMinVersion
	if m.Driver != nil && driverAtLeast(m.Driver.MinVersion, a.driver.MinVersion) {
		a.driver.MinVersion = m.Driver.MinVersion
	}
	a.refreshDriverLocked()
	if !rel.Newer {
		a.update.State, a.update.Available, a.update.Notes, a.update.PublishedAt = "up_to_date", nil, nil, nil
		a.upd.rel, a.upd.staged = nil, ""
		return
	}
	if rel.Artifact == nil {
		a.update.State = "failed"
		a.update.Error = &Problem{"no_artifact", "The new version has no download for this computer yet."}
		return
	}
	v, notes, pub := m.Version, m.Notes, m.PublishedAt
	a.update.Available, a.update.Notes, a.update.PublishedAt = &v, &notes, &pub
	if a.upd.rel == nil || a.upd.rel.Manifest.Version != v {
		a.upd.staged = ""
	}
	a.upd.rel = rel
	if m.Driver != nil && a.driver.State != "not_needed" && !driverAtLeast(a.driver.Version, m.Driver.MinVersion) {
		a.update.DriverRequired = &DriverRequirement{Name: m.Driver.Name, MinVersion: m.Driver.MinVersion}
	}
	if a.upd.staged != "" {
		a.update.State = "ready"
	} else {
		a.update.State = "available"
	}
	if rel.PreviouslyFailed != "" {
		a.update.Error = &Problem{"rolled_back", "Version " + v + " did not start properly here and was rolled back."}
	}
}

func (a *Agent) downloadUpdate(ctx context.Context) error {
	a.mu.Lock()
	rel := a.upd.rel
	if rel == nil || a.upd.running {
		a.mu.Unlock()
		return newAPIError(409, "no_update", "There is no update to download.")
	}
	a.upd.running = true
	a.update.State = "downloading"
	a.update.Progress = &Progress{BytesTotal: rel.Artifact.Size}
	a.mu.Unlock()
	a.publishUpdate()
	staged, err := a.opts.Updater.Download(ctx, rel, func(done, total int64) {
		a.mu.Lock()
		a.update.Progress = &Progress{BytesDone: done, BytesTotal: total}
		u := a.update
		a.mu.Unlock()
		a.hub.Throttle("update", "progress", u)
	})
	a.mu.Lock()
	a.upd.running = false
	if err != nil {
		a.update.State = "failed"
		a.update.Error = updateProblem(err)
	} else {
		a.upd.staged = staged
		a.update.State = "ready"
	}
	a.mu.Unlock()
	if err != nil {
		a.log.Warn("update download failed", "err", err)
	}
	a.publishUpdate()
	a.emitState()
	if err == nil {
		a.applyIfIdle()
	}
	return err
}

// ApplyUpdate asks for the staged update to be applied when idle or now.
func (a *Agent) ApplyUpdate(ctx context.Context, when string) (Update, error) {
	a.mu.Lock()
	if a.upd.rel == nil || a.update.Available == nil {
		a.mu.Unlock()
		return Update{}, newAPIError(409, "no_update", "There is no update to apply.")
	}
	if a.update.DriverRequired != nil {
		a.mu.Unlock()
		return Update{}, newAPIError(409, "driver_outdated", "Install the newer WinFsp driver first.")
	}
	a.upd.when = when
	if when == "now" {
		a.update.Apply = "now"
	}
	staged := a.upd.staged
	u := a.update
	a.mu.Unlock()
	if staged == "" {
		go a.downloadUpdate(context.WithoutCancel(ctx))
		return u, nil
	}
	a.applyIfIdle()
	a.mu.Lock()
	u = a.update
	a.mu.Unlock()
	return u, nil
}

// applyIfIdle exits Run for the update when one is staged and either the
// person asked for "now", or nothing is open and nothing is transferring.
func (a *Agent) applyIfIdle() {
	a.mu.Lock()
	if a.upd.staged == "" || a.update.DriverRequired != nil || a.update.State != "ready" {
		a.mu.Unlock()
		return
	}
	if a.upd.rel.PreviouslyFailed != "" && a.upd.when == "" {
		a.mu.Unlock()
		return // a version that failed here is applied only when asked
	}
	now := a.upd.when == "now"
	sum := a.opts.FS.Status().Summary
	a.mu.Unlock()
	if !now && (a.opts.Mounter.OpenFiles() > 0 || sum.Active+sum.Waiting > 0) {
		return
	}
	a.mu.Lock()
	a.update.State = "applying"
	pending := &PendingUpdate{
		Version:    a.upd.rel.Manifest.Version,
		Staged:     a.upd.staged,
		WasMounted: a.mount.State == MountMounted,
	}
	a.mu.Unlock()
	a.log.Info("applying update", "version", pending.Version)
	a.publishUpdate()
	a.emitState()
	a.requestExit(Exit{Update: pending})
}

func (a *Agent) publishUpdate() {
	a.mu.Lock()
	u := a.update
	a.mu.Unlock()
	a.hub.PublishNow("update", "progress", u)
}

func updateProblem(err error) *Problem {
	switch {
	case errors.Is(err, update.ErrBadSignature), errors.Is(err, update.ErrUnknownKey), errors.Is(err, update.ErrNoKeys):
		return &Problem{"bad_signature", "The update could not be verified."}
	case errors.Is(err, update.ErrReplayed):
		return &Problem{"replayed", "The update server offered an older release than one already seen."}
	case errors.Is(err, update.ErrHashMismatch), errors.Is(err, update.ErrSizeMismatch):
		return &Problem{"hash_mismatch", "The downloaded update was damaged; it will be fetched again."}
	case errors.Is(err, update.ErrNoArtifact):
		return &Problem{"no_artifact", "The new version has no download for this computer yet."}
	case errors.Is(err, update.ErrInvalid):
		return &Problem{"invalid", "The update description was not valid."}
	}
	return &Problem{"offline", "Could not check for updates."}
}
