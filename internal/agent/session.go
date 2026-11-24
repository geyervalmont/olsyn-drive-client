package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// apiError is an IPC error with its HTTP status and code.
type apiError struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, msg string) *apiError {
	return &apiError{Status: status, Code: code, Message: msg}
}

var (
	errAlreadySignedIn = newAPIError(409, "already_signed_in", "This device is already signed in.")
	errNotSignedIn     = newAPIError(409, "not_signed_in", "Sign in first.")
	errLinking         = newAPIError(409, "linking", "Sign-in is starting; try again in a moment.")
	errCredentialStore = newAPIError(503, "credential_store",
		"This computer's credential store can't keep a sign-in in this session (for example over SSH). Sign in from the desktop.")
)

// --- sign-in ---

func (a *Agent) credTarget(server string) (string, error) {
	return platform.CredentialTarget(server)
}

// resumeAccount starts the session for a saved account, or settles signed out.
func (a *Agent) resumeAccount(ctx context.Context) {
	a.mu.Lock()
	acc := a.account
	a.mu.Unlock()
	if acc == nil {
		a.mu.Lock()
		a.phase, a.ready = phaseSignedOut, true
		a.mu.Unlock()
		a.emitState()
		return
	}
	target, err := a.credTarget(acc.Server)
	var tok []byte
	if err == nil {
		tok, err = a.opts.Creds.Get(target)
	}
	if err != nil {
		reason := &Problem{"token_missing", "The sign-in for this device was not found. Sign in again."}
		if errors.Is(err, platform.ErrNotImplemented) {
			reason = &Problem{"credential_store", "This computer has no supported credential store yet."}
		}
		a.log.Warn("no token for saved account", "err", err)
		a.mu.Lock()
		a.account, a.phase, a.reason, a.ready = nil, phaseSignedOut, reason, true
		a.mu.Unlock()
		a.saveAccount(nil)
		a.emitState()
		return
	}
	a.mu.Lock()
	a.token = string(tok)
	a.mu.Unlock()
	a.startSession(ctx)
}

// SignIn starts a device link, or returns the one in progress.
func (a *Agent) SignIn(ctx context.Context, openBrowser bool) (SignIn, error) {
	a.mu.Lock()
	switch a.phase {
	case phaseLinking:
		if a.signin != nil && a.signin.State == "waiting" {
			s := *a.signin
			a.mu.Unlock()
			return s, nil
		}
		a.mu.Unlock()
		return SignIn{}, errLinking
	case phaseSignedOut:
	default:
		a.mu.Unlock()
		return SignIn{}, errAlreadySignedIn
	}
	a.mu.Unlock()
	// Refuse at once if the token could not be kept: otherwise the person
	// approves in the browser and the sign-in is then lost.
	if c, ok := a.opts.Creds.(platform.CredentialChecker); ok {
		if err := c.CheckWritable(); err != nil {
			a.log.Warn("sign-in refused: the credential store cannot keep a token", "err", err)
			return SignIn{}, errCredentialStore
		}
	}
	a.mu.Lock()
	if a.phase != phaseSignedOut {
		a.mu.Unlock()
		return SignIn{}, errLinking
	}
	a.phase, a.reason, a.signin = phaseLinking, nil, nil
	server, machine := a.settings.ServerURL, a.deviceNameLocked()
	runCtx := a.ctx
	a.mu.Unlock()
	a.emitState()

	startCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	drive := a.opts.DriveFor(server)
	link, err := drive.LinkStart(startCtx, LinkRequest{
		Client: ClientName, Machine: machine, AppVersion: a.opts.Version, Platform: platform.OSFamily(),
	})
	if err != nil {
		a.log.Warn("device link could not start", "err", err)
		a.mu.Lock()
		a.phase = phaseSignedOut
		a.mu.Unlock()
		a.emitState()
		return SignIn{}, driveCallError(err)
	}
	interval := max(link.Interval, a.opts.MinLinkInterval)
	expires := time.Now().UTC().Add(link.ExpiresIn)
	if link.ExpiresIn <= 0 {
		expires = time.Now().UTC().Add(10 * time.Minute)
	}
	if runCtx == nil {
		runCtx = context.Background()
	}
	linkCtx, linkCancel := context.WithDeadline(runCtx, expires)
	si := SignIn{State: "waiting", UserCode: link.UserCode, VerificationURL: link.VerificationURL, ExpiresAt: expires}
	a.mu.Lock()
	a.signin, a.linkCancel = &si, linkCancel
	a.mu.Unlock()
	a.log.Info("device link started", "server", server, "expires_at", expires)
	a.hub.Publish("signin", si)
	a.emitState()
	go a.pollLink(linkCtx, drive, link.DeviceCode, interval)
	if openBrowser {
		if err := a.opts.System.OpenURL(ctx, link.VerificationURL); err != nil {
			a.log.Warn("could not open the browser", "err", err)
		}
	}
	return si, nil
}

// CancelSignIn stops a device link in progress.
func (a *Agent) CancelSignIn() (SignIn, error) {
	a.mu.Lock()
	if a.phase != phaseLinking || a.linkCancel == nil {
		a.mu.Unlock()
		return SignIn{}, errNotSignedIn
	}
	a.linkCancel()
	s := a.finishLinkLocked("cancelled", nil)
	a.mu.Unlock()
	a.hub.Publish("signin", s)
	a.emitState()
	return s, nil
}

// finishLinkLocked ends linking without a token.
func (a *Agent) finishLinkLocked(state string, reason *Problem) SignIn {
	s := SignIn{State: state}
	if a.signin != nil {
		s = *a.signin
		s.State = state
	}
	if reason != nil && state != "cancelled" {
		s.Error = reason
	}
	a.signin, a.linkCancel = &s, nil
	a.phase, a.reason = phaseSignedOut, reason
	return s
}

func (a *Agent) pollLink(ctx context.Context, drive Drive, deviceCode string, interval time.Duration) {
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				a.endLink("expired", &Problem{"link_expired", "The sign-in code expired. Start again."})
			}
			return
		case <-t.C:
		}
		res, err := drive.LinkPoll(ctx, deviceCode)
		var ra *RetryAfterError
		switch {
		case err == nil:
			a.completeLink(ctx, res)
			return
		case errors.Is(err, ErrAuthorizationPending):
		case errors.As(err, &ra):
			interval += max(ra.After, 5*time.Second)
		case errors.Is(err, ErrLinkDenied):
			a.endLink("denied", &Problem{"link_denied", "Sign-in was declined in the browser."})
			return
		case errors.Is(err, ErrLinkExpired):
			a.endLink("expired", &Problem{"link_expired", "The sign-in code expired. Start again."})
			return
		case ctx.Err() != nil:
			continue // handled at the top of the loop
		default:
			a.log.Info("device link poll failed; retrying", "err", err)
		}
		t.Reset(interval)
	}
}

func (a *Agent) endLink(state string, reason *Problem) {
	a.mu.Lock()
	if a.phase != phaseLinking {
		a.mu.Unlock()
		return
	}
	s := a.finishLinkLocked(state, reason)
	a.mu.Unlock()
	a.log.Info("device link ended", "state", state)
	a.hub.Publish("signin", s)
	a.emitState()
}

func (a *Agent) completeLink(ctx context.Context, res LinkResult) {
	a.mu.Lock()
	if a.phase != phaseLinking {
		a.mu.Unlock()
		return
	}
	server := a.settings.ServerURL
	a.mu.Unlock()
	fail := func(p *Problem) {
		a.endLink("failed", p)
	}
	target, err := a.credTarget(server)
	if err == nil {
		err = a.opts.Creds.Set(target, []byte(res.Token))
	}
	if err != nil {
		a.log.Error("could not store the device token", "err", err)
		// The token is useless if it cannot be kept; tell the drive to drop it.
		unlinkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		a.opts.DriveFor(server).Unlink(unlinkCtx, res.Token, res.DeviceID)
		cancel()
		fail(&Problem{"credential_store", "The sign-in could not be saved in this computer's credential store."})
		return
	}
	acc := &Account{
		Server: server, UserID: res.User.ID, Name: res.User.Name, Email: res.User.Email,
		DeviceID: res.DeviceID, ReadOnly: res.ReadOnly, LinkedAt: time.Now().UTC(),
	}
	if err := a.saveAccount(acc); err != nil {
		a.log.Error("could not save the account", "err", err)
	}
	a.mu.Lock()
	s := SignIn{State: "approved"}
	if a.signin != nil {
		s = *a.signin
		s.State = "approved"
	}
	a.signin, a.linkCancel = &s, nil
	a.account, a.token, a.reason = acc, res.Token, nil
	a.mountWanted, a.linkedOnce = true, true
	a.saveAgentStateLocked()
	runCtx := a.ctx
	a.mu.Unlock()
	a.log.Info("device linked", "device_id", res.DeviceID)
	a.hub.Publish("signin", s)
	if runCtx == nil {
		runCtx = context.Background()
	}
	a.startSession(runCtx)
}

// --- session ---

// startSession brings the file system up, then the mount, then heartbeats.
func (a *Agent) startSession(ctx context.Context) {
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	a.mu.Lock()
	a.phase, a.ready, a.net = phaseStarting, false, Online
	a.sessionCancel, a.sessionDone = cancel, done
	sess := Session{
		ServerURL: a.account.Server, Token: a.token, DeviceID: a.account.DeviceID, UserID: a.account.UserID,
		ReadOnly: a.account.ReadOnly, CacheDir: a.settings.CacheDir, CacheCapBytes: a.settings.CacheCapBytes,
		Bandwidth: a.settings.Bandwidth,
	}
	a.mu.Unlock()
	a.emitState()
	go func() {
		defer close(done)
		a.runSession(sctx, sess)
	}()
}

func (a *Agent) runSession(ctx context.Context, sess Session) {
	backoff := time.Second
	for {
		err := a.opts.FS.Start(ctx, sess)
		if err == nil {
			break
		}
		a.log.Warn("file system did not start; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	if !a.heartbeat(ctx) {
		return // revoked: the session was torn down
	}
	a.mu.Lock()
	a.phase = phaseRunning
	wantMount := a.mountWanted && a.letterConfirmed
	if a.mountWanted && !a.letterConfirmed {
		a.log.Info("not mounting until the drive letter is chosen")
	}
	a.mu.Unlock()
	a.emitState()
	if wantMount {
		if err := a.canMount(); err == nil {
			a.doMount(ctx)
		} else {
			a.log.Info("not mounting at start", "reason", err)
		}
	}
	a.mu.Lock()
	a.ready = true
	a.mu.Unlock()
	a.emitState()

	t := time.NewTicker(a.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !a.heartbeat(ctx) {
				return
			}
		}
	}
}

// heartbeat reports this device once. It returns false when the token was
// revoked and the session has been ended.
func (a *Agent) heartbeat(ctx context.Context) bool {
	a.mu.Lock()
	if a.account == nil {
		a.mu.Unlock()
		return false
	}
	tok, server := a.token, a.account.Server
	hb := Heartbeat{State: a.mount.State, MountPath: a.mount.Path, AppVersion: a.opts.Version, Platform: platform.OSFamily()}
	if a.mount.Error != nil {
		hb.ErrorCode = a.mount.Error.Code
	}
	a.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := a.opts.DriveFor(server).Heartbeat(hctx, tok, hb)
	cancel()
	var ra *RetryAfterError
	next := Online
	switch {
	case err == nil:
	case errors.Is(err, ErrUnauthenticated):
		a.revoked(ctx)
		return false
	case errors.As(err, &ra):
		next = ServerRestarting
	case ctx.Err() != nil:
		return true
	default:
		next = Offline
	}
	a.mu.Lock()
	changed := a.net != next
	a.net = next
	a.mu.Unlock()
	if changed {
		a.log.Info("drive connectivity changed", "now", next)
		a.emitState()
	}
	return true
}

// revoked handles a token the drive no longer accepts: signed out by the drive.
func (a *Agent) revoked(ctx context.Context) {
	a.log.Warn("device token revoked by the drive; signing out")
	go func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		a.teardown(stopCtx, false)
		a.mu.Lock()
		a.reason = &Problem{"token_revoked", "Signed out by the drive. Sign in again to reconnect."}
		a.ready = true
		a.mu.Unlock()
		a.emitState()
		a.notify(Notification{Kind: "signed_out", Title: "Signed out of Olsyn Drive",
			Body:   "This computer was removed from your drive. Sign in again to reconnect.",
			Action: &NotificationAction{Label: "Sign in", Open: OpenAction{Target: "ui", Path: "signin"}}})
	}()
}

// teardown stops the session, unmounts, and forgets the token and account.
// It does not touch the cache.
func (a *Agent) teardown(ctx context.Context, fromSessionGoroutine bool) {
	a.stopSession(ctx, !fromSessionGoroutine)
	a.mu.Lock()
	acc := a.account
	a.account, a.token, a.phase, a.net = nil, "", phaseSignedOut, Online
	a.mu.Unlock()
	if acc != nil {
		if target, err := a.credTarget(acc.Server); err == nil {
			if err := a.opts.Creds.Delete(target); err != nil {
				a.log.Warn("could not delete the device token", "err", err)
			}
		}
	}
	if err := a.saveAccount(nil); err != nil {
		a.log.Warn("could not remove the account file", "err", err)
	}
}

// stopSession cancels the session goroutine, unmounts and stops the file system.
func (a *Agent) stopSession(ctx context.Context, wait bool) {
	a.mu.Lock()
	cancel, done := a.sessionCancel, a.sessionDone
	a.sessionCancel, a.sessionDone = nil, nil
	if a.linkCancel != nil {
		a.linkCancel()
		a.linkCancel = nil
	}
	if a.pauseTimer != nil {
		a.pauseTimer.Stop()
		a.pauseTimer = nil
	}
	mounted := a.mount.State == MountMounted || a.mount.State == MountMounting
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if wait && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	if mounted {
		if err := a.opts.Mounter.Unmount(ctx, true); err != nil {
			a.log.Warn("unmount failed", "err", err)
		}
		a.mu.Lock()
		a.mount.State, a.mount.Path, a.mount.Error = MountUnmounted, "", nil
		a.mu.Unlock()
	}
	if err := a.opts.FS.Stop(ctx); err != nil {
		a.log.Warn("file system stop failed", "err", err)
	}
	a.mu.Lock()
	a.paused, a.pausedUntil = false, nil
	a.mu.Unlock()
}

// SignOut unlinks this device, removes the token, unmounts, and clears the
// cache if asked, refusing to drop unsent files unless confirmed.
func (a *Agent) SignOut(ctx context.Context, clearCache, discardConfirmed bool) (SignOutResult, error) {
	a.mu.Lock()
	phase := a.phase
	a.mu.Unlock()
	switch phase {
	case phaseLinking:
		a.CancelSignIn()
		return SignOutResult{SignedOut: true}, nil
	case phaseSignedOut, phaseStopping:
		return SignOutResult{}, errNotSignedIn
	}
	unsent, err := a.opts.FS.Unsent(ctx)
	if err != nil {
		return SignOutResult{}, fmt.Errorf("count unsent files: %w", err)
	}
	pending := unsent.Files + unsent.Held
	if clearCache && pending > 0 && !discardConfirmed {
		e := newAPIError(409, "unsent_data", fmt.Sprintf("%s (%s) have not reached the drive yet.", plural(pending, "file", "files"), humanBytes(unsent.Bytes)))
		e.Extra = map[string]any{"unsent": unsent}
		return SignOutResult{}, e
	}
	a.mu.Lock()
	tok, acc := a.token, a.account
	cacheDir := a.settings.CacheDir
	a.mu.Unlock()

	a.stopSession(ctx, true)
	res := SignOutResult{SignedOut: true}
	if acc != nil {
		uctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.opts.DriveFor(acc.Server).Unlink(uctx, tok, acc.DeviceID)
		cancel()
		if err != nil && !errors.Is(err, ErrUnauthenticated) {
			msg := "The drive could not be reached; remove this computer from your devices in the web app."
			res.UnlinkError = &msg
			a.log.Warn("unlink failed", "err", err)
		} else {
			res.Unlinked = true
		}
	}
	a.teardown(ctx, false)
	if clearCache {
		if err := a.opts.FS.ClearCache(ctx, cacheDir); err != nil {
			a.log.Warn("clear cache failed", "err", err)
		} else {
			res.CacheCleared = true
			a.activity.clear() // it names files of the account that left
		}
	} else if pending > 0 {
		res.UnsentKept = &unsent
	}
	a.mu.Lock()
	a.reason = &Problem{"signed_out", "Signed out."}
	a.mountWanted = true
	a.saveAgentStateLocked()
	a.ready = true
	a.mu.Unlock()
	a.log.Info("signed out", "cache_cleared", res.CacheCleared, "unlinked", res.Unlinked)
	a.emitState()
	return res, nil
}

// --- mount ---

func (a *Agent) canMount() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseRunning && a.phase != phaseStarting {
		return errNotSignedIn
	}
	switch a.driver.State {
	case "missing", "installing":
		return newAPIError(409, "driver_missing", "Install the WinFsp driver first.")
	case "outdated":
		return newAPIError(409, "driver_outdated", "A newer WinFsp driver is needed.")
	}
	if a.update.Required {
		return newAPIError(409, "update_required", "Update Olsyn Drive to connect the drive.")
	}
	return nil
}

// Mount connects the drive letter (asynchronously; progress via state
// events). letter, when set, is the person's choice: it must be free, it is
// saved as the drive_letter setting, and (like any explicit mount) it
// confirms the letter, so the agent mounts at start from then on.
func (a *Agent) Mount(ctx context.Context, letter string) (Mount, bool, error) {
	if err := a.canMount(); err != nil {
		return Mount{}, false, err
	}
	if letter != "" {
		letter = settings.NormalizeDriveLetter(letter)
		if len(letter) != 2 || letter[0] < 'D' || letter[0] > 'Z' {
			return Mount{}, false, invalid("letter", "Choose a letter from D: to Z:.")
		}
	}
	inUse, err := a.opts.System.DriveLettersInUse()
	if err != nil {
		a.log.Warn("could not list drive letters", "err", err)
	}
	a.mu.Lock()
	if a.mount.State == MountMounted || a.mount.State == MountMounting {
		m := a.mountStateLocked()
		a.mu.Unlock()
		if letter != "" && m.Letter != nil && *m.Letter != letter {
			return Mount{}, false, newAPIError(409, "mounted", "The drive is already connected as "+*m.Letter+". Change the letter in Settings.")
		}
		return m, true, nil
	}
	if letter != "" {
		if kind := inUse[letter]; kind != "" {
			a.mu.Unlock()
			e := newAPIError(409, "letter_taken", letter+" is already used on this PC by "+letterUser(kind)+". Choose another letter.")
			e.Extra = map[string]any{"letter": letter, "kind": kind}
			return Mount{}, false, e
		}
		if letter != a.settings.DriveLetter {
			next := a.settings
			next.DriveLetter = letter
			if err := settings.Save(a.settingsPath, next); err != nil {
				a.mu.Unlock()
				return Mount{}, false, err
			}
			a.settings = next
			a.mount.Letter = letterPtr(letter)
			a.hub.Publish("settings", next)
		}
	}
	a.mountWanted, a.letterConfirmed = true, true
	a.saveAgentStateLocked()
	runCtx := a.ctx
	a.mu.Unlock()
	go a.doMount(runCtx)
	a.mu.Lock()
	m := a.mount
	a.mu.Unlock()
	return m, false, nil
}

func (a *Agent) doMount(ctx context.Context) {
	a.mu.Lock()
	if a.mount.State == MountMounted || a.mount.State == MountMounting {
		a.mu.Unlock()
		return
	}
	letter := a.settings.DriveLetter
	a.mount = Mount{State: MountMounting, Letter: letterPtr(letter)}
	a.mu.Unlock()
	a.emitState()
	if inUse, err := a.opts.System.DriveLettersInUse(); err == nil && inUse[letter] != "" {
		// taken since it was chosen (a network drive mapped meanwhile, say):
		// say so plainly instead of a driver error
		a.mu.Lock()
		a.mount = Mount{State: MountFailed, Letter: letterPtr(letter), Error: &Problem{"letter_taken",
			letter + " is already used on this PC by " + letterUser(inUse[letter]) + ". Choose another letter in Settings."}}
		a.mu.Unlock()
		a.log.Warn("mount: letter taken", "letter", letter, "by", inUse[letter])
		a.emitState()
		return
	}
	mctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	path, err := a.opts.Mounter.Mount(mctx, letter)
	cancel()
	a.mu.Lock()
	if err != nil {
		a.mount = Mount{State: MountFailed, Letter: letterPtr(letter), Error: &Problem{"mount_failed", "The drive could not be connected as " + letter + ": " + err.Error()}}
	} else {
		a.mount = Mount{State: MountMounted, Letter: letterPtr(letter), Path: path}
	}
	a.mu.Unlock()
	if err != nil {
		a.log.Warn("mount failed", "letter", letter, "err", err)
	} else {
		a.log.Info("mounted", "letter", letter)
	}
	a.emitState()
}

// Unmount disconnects the drive letter.
func (a *Agent) Unmount(ctx context.Context, force bool) (Mount, error) {
	a.mu.Lock()
	if a.mount.State != MountMounted && a.mount.State != MountFailed {
		a.mu.Unlock()
		return Mount{}, newAPIError(409, "not_mounted", "The drive is not connected.")
	}
	a.mu.Unlock()
	if n := a.opts.Mounter.OpenFiles(); n > 0 && !force {
		e := newAPIError(409, "busy", plural(n, "file is", "files are")+" open on the drive.")
		e.Extra = map[string]any{"open_files": n}
		return Mount{}, e
	}
	a.mu.Lock()
	a.mountWanted = false
	a.saveAgentStateLocked()
	wasFailed := a.mount.State == MountFailed
	a.mount.State = MountUnmounting
	m := a.mount
	runCtx := a.ctx
	a.mu.Unlock()
	a.emitState()
	go func() {
		var err error
		if !wasFailed {
			err = a.opts.Mounter.Unmount(runCtx, force)
		}
		a.mu.Lock()
		if err != nil {
			a.mount.State = MountMounted
			a.mount.Error = &Problem{"unmount_failed", err.Error()}
		} else {
			a.mount = Mount{State: MountUnmounted, Letter: letterPtr(a.settings.DriveLetter)}
		}
		a.mu.Unlock()
		a.emitState()
	}()
	return m, nil
}

// --- pause ---

// Pause stops transfers, optionally for a number of minutes.
func (a *Agent) Pause(ctx context.Context, minutes int) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if err := a.opts.FS.Pause(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	a.mu.Lock()
	a.paused, a.pausedUntil = true, nil
	if a.pauseTimer != nil {
		a.pauseTimer.Stop()
		a.pauseTimer = nil
	}
	if minutes > 0 {
		until := time.Now().UTC().Add(time.Duration(minutes) * time.Minute)
		a.pausedUntil = &until
		a.pauseTimer = time.AfterFunc(time.Until(until), func() {
			a.Resume(context.Background())
		})
	}
	a.mu.Unlock()
	a.emitState()
	return nil
}

// Resume restarts transfers.
func (a *Agent) Resume(ctx context.Context) error {
	if err := a.requireRunning(); err != nil {
		return err
	}
	if err := a.opts.FS.Resume(ctx); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	a.mu.Lock()
	a.paused, a.pausedUntil = false, nil
	if a.pauseTimer != nil {
		a.pauseTimer.Stop()
		a.pauseTimer = nil
	}
	a.mu.Unlock()
	a.emitState()
	return nil
}

func (a *Agent) requireRunning() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != phaseRunning && a.phase != phaseStarting {
		return errNotSignedIn
	}
	return nil
}

// --- helpers ---

// driveCallError turns a failed drive call into an IPC error.
func driveCallError(err error) error {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		e := newAPIError(503, "server_restarting", "The drive is restarting; try again shortly.")
		e.Extra = map[string]any{"retry_after": int(ra.After.Seconds())}
		return e
	}
	return newAPIError(503, "offline", "The drive could not be reached.")
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// drivePathToLocal maps a drive path onto the mount, refusing escapes.
func drivePathToLocal(mountPath, drivePath string) string {
	// Cleaning a rooted path removes every "..", so the result stays under mountPath.
	clean := filepath.Clean("/" + strings.TrimPrefix(filepath.ToSlash(drivePath), "/"))
	return filepath.Join(mountPath, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
}

// mountStateLocked is the mount as the API shows it.
func (a *Agent) mountStateLocked() Mount {
	m := a.mount
	m.LetterConfirmed = a.letterConfirmed
	return m
}

// letterUser names what holds a drive letter, for messages.
func letterUser(kind string) string {
	switch kind {
	case platform.LetterNetwork:
		return "a network drive"
	case platform.LetterSubst:
		return "a folder shown as a drive (subst)"
	}
	return "another drive"
}
