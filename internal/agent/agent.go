// Package agent is the per-user daemon: sign-in state, session start and
// stop, heartbeat, mount control, settings, updates, and the IPC server that
// implements docs/ipc.md for the UI and the CLI.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// Defaults for Options.
const (
	DefaultHeartbeatInterval = 30 * time.Second
	DefaultUpdateInterval    = 4 * time.Hour
	DefaultUpdateFirstCheck  = 2 * time.Minute
	ClientName               = "olsyn-drive"
)

// Options wires the agent. Listener, DriveFor, FS, Mounter, Creds and System are required.
type Options struct {
	Version  string
	Platform string // release platform name; default platform.Name()
	Paths    platform.Paths
	Listener net.Listener
	// DriveFor returns the drive API client for a server URL (the settings'
	// server while linking, the account's server once signed in).
	DriveFor func(serverURL string) Drive
	FS       FileSystem
	Mounter  Mounter
	Creds    platform.CredentialStore
	System   System
	Updater  Updater // nil disables updates
	Logger   *slog.Logger

	// Server, when set, is the only drive server this agent talks to: the
	// settings' server_url shows it and cannot be changed. Release builds set
	// it to the product's drive; developer builds to --server, if given.
	Server string
	// DeviceName defaults to platform.ComputerName().
	DeviceName string
	// HeartbeatInterval defaults to 30 s.
	HeartbeatInterval time.Duration
	// MinLinkInterval is the shortest link poll interval honoured (default 1 s).
	MinLinkInterval time.Duration
	// UpdateInterval and UpdateFirstCheck schedule update checks.
	UpdateInterval   time.Duration
	UpdateFirstCheck time.Duration
	// UpdatedFrom is the version this agent replaced (after a healthy update).
	UpdatedFrom string
	// Licences is the open-source notice text served at GET /v1/licences.
	Licences string
	// RolledBack is set when this agent was restarted after a failed update.
	RolledBack *Problem
	// OnReady is called once start-up has finished (update health, cleanup).
	OnReady func()
}

// Agent is the daemon. Create it with New and start it with Run.
type Agent struct {
	opts  Options
	log   *slog.Logger
	hub   *Hub
	run   string
	start time.Time

	settingsPath string
	accountPath  string
	statePath    string

	mu             sync.Mutex
	settings       settings.Settings
	defaults       settings.Settings
	pendingRestart []string
	phase          string // signed_out, linking, starting, running, stopping
	reason         *Problem
	account        *Account
	token          string // never logged, never sent over IPC
	signin         *SignIn
	linkCancel     context.CancelFunc
	mount          Mount
	mountWanted    bool
	paused         bool
	pausedUntil    *time.Time
	pauseTimer     *time.Timer
	net            string // from heartbeats: Online, Offline, ServerRestarting
	update         Update
	upd            updateState
	driver         Driver
	ready          bool
	readyFired     bool
	sessionCancel  context.CancelFunc
	sessionDone    chan struct{}
	lastState      []byte
	transferStates map[string]string
	notifySeq      int
	linkedOnce     bool
	activity       *activityLog
	exit           chan Exit
	exitOnce       sync.Once
	ctx            context.Context // Run's context, for background work
}

// Phases.
const (
	phaseSignedOut = "signed_out"
	phaseLinking   = "linking"
	phaseStarting  = "starting"
	phaseRunning   = "running"
	phaseStopping  = "stopping"
)

// agentState is persisted between runs.
type agentState struct {
	MountWanted bool `json:"mount_wanted"`
	// LinkedOnce: this PC has been signed in at least once (first_run off).
	LinkedOnce bool `json:"linked_once"`
}

// New loads settings and the account, and prepares the agent.
func New(opts Options) (*Agent, error) {
	if opts.Listener == nil || opts.DriveFor == nil || opts.FS == nil || opts.Mounter == nil || opts.Creds == nil || opts.System == nil {
		return nil, errors.New("agent: Listener, DriveFor, FS, Mounter, Creds and System are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Platform == "" {
		opts.Platform = platform.Name()
	}
	if opts.DeviceName == "" {
		opts.DeviceName = platform.ComputerName()
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.MinLinkInterval <= 0 {
		opts.MinLinkInterval = time.Second
	}
	if opts.UpdateInterval <= 0 {
		opts.UpdateInterval = DefaultUpdateInterval
	}
	if opts.UpdateFirstCheck <= 0 {
		opts.UpdateFirstCheck = DefaultUpdateFirstCheck
	}
	if err := opts.Paths.Ensure(); err != nil {
		return nil, err
	}
	run := randomID(4)
	a := &Agent{
		opts:           opts,
		log:            opts.Logger,
		hub:            NewHub(run),
		run:            run,
		start:          time.Now().UTC(),
		settingsPath:   filepath.Join(opts.Paths.Config, "settings.json"),
		accountPath:    filepath.Join(opts.Paths.Config, "account.json"),
		statePath:      filepath.Join(opts.Paths.State, "agent-state.json"),
		activity:       openActivity(filepath.Join(opts.Paths.State, "activity.json")),
		phase:          phaseSignedOut,
		net:            Online,
		mountWanted:    true,
		transferStates: map[string]string{},
		exit:           make(chan Exit, 1),
	}
	env := settings.Env{CacheDir: opts.Paths.Cache}
	if free, err := opts.System.FreeDiskBytes(opts.Paths.Cache); err == nil {
		env.FreeDiskBytes = free
	}
	s, created, err := settings.Load(a.settingsPath, env)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	if created {
		if err := settings.Save(a.settingsPath, s); err != nil {
			return nil, fmt.Errorf("save first settings: %w", err)
		}
	}
	if opts.Server != "" {
		s.ServerURL = opts.Server // effective value, not written to the file
	}
	a.settings, a.defaults = s, settings.Defaults(env)
	a.mount = Mount{State: MountUnmounted, Letter: letterPtr(s.DriveLetter)}
	a.notifySeq = a.activity.lastID()
	a.update = Update{State: "idle", Channel: s.UpdateChannel, Current: opts.Version, Apply: "when_idle"}
	if opts.RolledBack != nil {
		a.update.State = "failed"
		a.update.Error = opts.RolledBack
	}
	if st, err := a.loadAgentState(); err == nil {
		a.mountWanted = st.MountWanted
		a.linkedOnce = st.LinkedOnce
	}
	acc, err := a.loadAccount()
	if err != nil {
		a.log.Warn("account file unreadable; starting signed out", "err", err)
	}
	a.account = acc
	a.refreshDriverLocked()
	return a, nil
}

func letterPtr(l string) *string {
	if l == "" {
		return nil
	}
	return &l
}

func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Run serves IPC and runs the session until ctx ends, a quit request, or an
// update needs the process to restart. It always shuts down gracefully.
func (a *Agent) Run(ctx context.Context) (Exit, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()

	go a.hub.Run(ctx)
	if ch := a.opts.FS.Events(); ch != nil {
		go a.consumeFS(ctx, ch)
	}

	// Requests (event streams included) end when serving stops.
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	srv := &http.Server{
		Handler:           a.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return serveCtx },
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(a.opts.Listener) }()
	a.log.Info("agent started", "version", a.opts.Version, "platform", a.opts.Platform, "run", a.run)

	a.resumeAccount(ctx)
	if a.opts.Updater != nil {
		go a.updateLoop(ctx)
	}

	var exit Exit
	var err error
	select {
	case <-ctx.Done():
	case exit = <-a.exit:
	case e := <-serveErr:
		if e != nil && !errors.Is(e, http.ErrServerClosed) {
			err = fmt.Errorf("ipc server: %w", e)
		}
	}
	a.log.Info("agent stopping")
	a.mu.Lock()
	a.phase = phaseStopping
	a.mu.Unlock()
	stopServing()
	httpCtx, httpDone := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	if srv.Shutdown(httpCtx) != nil {
		srv.Close() // connections that never sent a request
	}
	httpDone()
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer done()
	a.stopSession(shutdownCtx, true)
	a.log.Info("agent stopped")
	return exit, err
}

// requestExit ends Run with e (once).
func (a *Agent) requestExit(e Exit) {
	a.exitOnce.Do(func() { a.exit <- e })
}

// --- state ---

// State returns the current state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

func (a *Agent) stateLocked() State {
	fs := a.opts.FS.Status()
	st := State{
		APIVersion:  APIVersion,
		APIVersions: []int{APIVersion},
		AgentRun:    a.run,
		Version:     a.opts.Version,
		Platform:    a.opts.Platform,
		Ready:       a.ready,
		Reason:      a.reason,
		Account:     a.account,
		DeviceName:  a.deviceNameLocked(),
		SignIn:      a.signin,
		Mount:       a.mount,
		Paused:      a.paused,
		PausedUntil: a.pausedUntil,
		Transfers:   fs.Summary,
		Update:      a.update,
		Driver:      a.driver,
		StartedAt:   a.start,
		FirstRun:    a.account == nil && !a.linkedOnce,
		Unsent:      fs.Unsent,
	}
	st.Issues = a.issuesLocked(fs)
	st.Connection = a.connectionLocked(fs, st.Issues)
	return st
}

func (a *Agent) deviceNameLocked() string {
	if a.settings.DeviceName != "" {
		return a.settings.DeviceName
	}
	return a.opts.DeviceName
}

func (a *Agent) connectionLocked(fs FSStatus, issues []Issue) string {
	switch a.phase {
	case phaseSignedOut, phaseStopping:
		if a.account == nil {
			return ConnSignedOut
		}
		return ConnStarting
	case phaseLinking:
		return ConnLinking
	case phaseStarting:
		return ConnStarting
	}
	switch {
	case a.net == Offline || fs.Connectivity == Offline:
		return ConnOffline
	case a.net == ServerRestarting || fs.Connectivity == ServerRestarting:
		return ConnServerRestarting
	case len(issues) > 0:
		return ConnAttention
	case a.paused:
		return ConnPaused
	case fs.Summary.Active+fs.Summary.Waiting > 0:
		return ConnWorking
	}
	return ConnConnected
}

func (a *Agent) issuesLocked(fs FSStatus) []Issue {
	issues := []Issue{}
	if a.phase != phaseRunning {
		return issues
	}
	if n := fs.Summary.Held; n > 0 {
		issues = append(issues, Issue{"held_files", plural(n, "file", "files") + " could not be saved to the drive.", "show_held"})
	}
	if a.mount.State == MountFailed {
		msg := "The drive could not be connected."
		if a.mount.Error != nil {
			msg = a.mount.Error.Message
		}
		issues = append(issues, Issue{"mount_failed", msg, "mount"})
	}
	switch a.driver.State {
	case "missing":
		issues = append(issues, Issue{"driver_missing", "The WinFsp driver is needed to show the drive. It installs with one administrator prompt.", "install_driver"})
	case "outdated":
		issues = append(issues, Issue{"driver_outdated", "A newer WinFsp driver is needed.", "install_driver"})
	}
	if a.update.Required {
		issues = append(issues, Issue{"update_required", "This version is no longer supported. Update to connect the drive.", "apply_update"})
	}
	if a.update.State == "failed" && a.update.Error != nil && a.update.Error.Code == "rolled_back" {
		issues = append(issues, Issue{"update_failed", a.update.Error.Message, "check_update"})
	}
	if fs.CacheFull {
		issues = append(issues, Issue{"cache_full", "The cache is full of files not yet sent; saving is slowed to upload speed.", "show_settings"})
	}
	return issues
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// emitState publishes a state event if anything but the summary changed.
// Callers must not hold a.mu.
func (a *Agent) emitState() {
	a.mu.Lock()
	st := a.stateLocked()
	cmp := st
	cmp.Transfers = Summary{}
	data, _ := json.Marshal(cmp)
	changed := string(data) != string(a.lastState)
	if changed {
		a.lastState = data
	}
	fire := a.ready && !a.readyFired
	if fire {
		a.readyFired = true
	}
	a.mu.Unlock()
	if changed {
		a.hub.Publish("state", st)
	}
	if fire && a.opts.UpdatedFrom != "" {
		a.notify(Notification{Kind: "update_applied", Title: "Olsyn Drive was updated",
			Body: "Now on version " + a.opts.Version + "."})
	}
	if fire && a.opts.RolledBack != nil {
		a.notify(Notification{Kind: "update_failed", Title: "The update was undone", Body: a.opts.RolledBack.Message})
	}
	if fire && a.opts.OnReady != nil {
		go a.opts.OnReady()
	}
}

// notify publishes a notification if the person wants that kind.
func (a *Agent) notify(n Notification) {
	a.mu.Lock()
	want := a.settings.Notifications
	a.notifySeq++
	n.ID = fmt.Sprintf("n_%d", a.notifySeq)
	a.mu.Unlock()
	if n.At.IsZero() {
		n.At = time.Now().UTC()
	}
	a.activity.add(n) // the activity list keeps every kind, shown or not
	switch n.Kind {
	case "finished", "conflict":
		if !want.Finished {
			return
		}
	case "held":
		if !want.Held {
			return
		}
	case "signed_out":
		if !want.SignedOut {
			return
		}
	case "update_applied", "update_failed":
		if !want.UpdateApplied {
			return
		}
	}
	if n.At.IsZero() {
		n.At = time.Now().UTC()
	}
	a.hub.Publish("notification", n)
}

// consumeFS turns file system events into IPC events.
func (a *Agent) consumeFS(ctx context.Context, ch <-chan FSEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			switch {
			case ev.Transfer != nil:
				t := *ev.Transfer
				a.mu.Lock()
				prev, seen := a.transferStates[t.ID]
				if t.Terminal() {
					delete(a.transferStates, t.ID)
				} else {
					a.transferStates[t.ID] = t.State
				}
				a.mu.Unlock()
				if !seen || prev != t.State {
					a.hub.PublishNow("transfer", t.ID, t)
				} else {
					a.hub.Throttle("transfer", t.ID, t)
				}
				a.hub.Throttle("summary", "summary", a.opts.FS.Status().Summary)
			case ev.Held != nil:
				a.hub.Publish("held", *ev.Held)
				a.emitState()
			case ev.Notification != nil:
				a.notify(*ev.Notification)
			case ev.Batch != nil:
				b := *ev.Batch
				if b.State == BatchSending || b.State == BatchScanning {
					a.hub.Throttle("batch", b.ID, b)
				} else {
					a.hub.PublishNow("batch", b.ID, b)
				}
				a.hub.Throttle("summary", "summary", a.opts.FS.Status().Summary)
			case ev.StatusChanged:
				a.hub.Throttle("summary", "summary", a.opts.FS.Status().Summary)
				a.emitState()
			}
		}
	}
}

// --- persistence ---

func (a *Agent) loadAccount() (*Account, error) {
	data, err := os.ReadFile(a.accountPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}
	var acc Account
	if err := json.Unmarshal(data, &acc); err != nil {
		return nil, fmt.Errorf("parse account: %w", err)
	}
	return &acc, nil
}

func (a *Agent) saveAccount(acc *Account) error {
	if acc == nil {
		if err := os.Remove(a.accountPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove account: %w", err)
		}
		return nil
	}
	data, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account: %w", err)
	}
	return settings.WriteFileAtomic(a.accountPath, data, 0o600)
}

func (a *Agent) loadAgentState() (agentState, error) {
	st := agentState{MountWanted: true}
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func (a *Agent) saveAgentStateLocked() {
	data, _ := json.Marshal(agentState{MountWanted: a.mountWanted, LinkedOnce: a.linkedOnce})
	if err := settings.WriteFileAtomic(a.statePath, data, 0o600); err != nil {
		a.log.Warn("could not save agent state", "err", err)
	}
}

// --- driver ---

func (a *Agent) refreshDriverLocked() {
	info, err := a.opts.System.DetectDriver()
	d := Driver{Name: info.Name, Version: info.Version, MinVersion: a.driver.MinVersion}
	switch {
	case err != nil:
		d.State = "unknown"
		d.Error = &Problem{"detect_failed", err.Error()}
	case !info.Needed:
		d.State = "not_needed"
	case !info.Installed:
		d.State = "missing"
	case d.MinVersion != "" && !driverAtLeast(info.Version, d.MinVersion):
		d.State = "outdated"
	default:
		d.State = "ok"
	}
	if a.driver.State == "installing" && d.State != "ok" {
		d.State = "installing"
	}
	a.driver = d
}

func (a *Agent) refreshDriver() {
	a.mu.Lock()
	a.refreshDriverLocked()
	a.mu.Unlock()
	a.emitState()
}
