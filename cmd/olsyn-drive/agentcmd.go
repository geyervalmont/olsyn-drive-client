package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/notices"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
	"github.com/geyervalmont/olsyn-drive-client/internal/winfspmsi"
)

// cmdAgent runs the per-user agent until signalled, quit over IPC, or
// stopped to apply an update (then it supervises the hand-off).
func cmdAgent(e *env, args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	afterUpdate := fs.String("after-update", "", "version this agent replaced (set by the updater)")
	rolledBack := fs.String("rolled-back", "", "version that failed its health check (set by the updater)")
	logStderr := fs.Bool("log-stderr", false, "also log to stderr")
	serverFlag := fs.String("server", "", "drive server (developer builds only; also "+serverEnv+")")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	server, err := chooseServer(*serverFlag)
	if err != nil {
		e.errorf("olsyn-drive: %v\n", err)
		return exitUsage
	}
	if err := e.paths.Ensure(); err != nil {
		e.errorf("olsyn-drive: %v\n", err)
		return exitError
	}
	logFile, err := openRotating(filepath.Join(e.paths.Logs, "agent.log"), 10<<20, 3)
	if err != nil {
		e.errorf("olsyn-drive: %v\n", err)
		return exitError
	}
	defer logFile.Close()
	var w io.Writer = logFile
	if *logStderr {
		w = io.MultiWriter(logFile, e.stderr)
	}
	log := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("agent starting", "version", version, "platform", platform.Name(), "dev_build", devBuild,
		"server", server, "state_dir", e.paths.State, "cache_dir", e.paths.Cache, "logs", e.paths.Logs)
	if drv, derr := platform.DetectDriver(); derr == nil {
		log.Info("file system driver", "name", drv.Name, "needed", drv.Needed, "installed", drv.Installed, "version", drv.Version)
	} else {
		log.Warn("file system driver could not be detected", "err", derr)
	}

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Error("cannot find own executable", "err", err)
		return exitError
	}

	release, err := platform.AcquireInstance(e.paths)
	if errors.Is(err, platform.ErrAlreadyRunning) {
		e.errorf("olsyn-drive: the agent is already running\n")
		return exitOK
	}
	if err != nil {
		log.Error("single instance lock failed", "err", err)
		return exitError
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	l, err := platform.Listen(e.endpoint)
	if err != nil {
		log.Error("cannot listen for the ui", "err", err)
		e.errorf("olsyn-drive: %v\n", err)
		return exitError
	}

	creds := platform.NewCredentialStore()
	devStore, err := devCredentialStore(e.paths)
	if err != nil {
		log.Error("cannot open the token store", "err", err)
		e.errorf("olsyn-drive: %v\n", err)
		l.Close()
		return exitError
	}
	if devStore != nil {
		creds = devStore
		log.Warn("dev build: the sign-in is kept in an owner-only file under the data folder, not the credential store")
	} else if devBuild && runtime.GOOS != "windows" {
		// macOS and Linux have no credential store yet: a dev build keeps
		// the sign-in in memory so the agent can be tried end to end.
		creds = platform.NewMemoryCredentialStore()
		log.Warn("dev build: the sign-in is kept in memory only and is lost when the agent stops")
	}
	hc := newHTTPClient()
	coreFS, err := newCore(e, hc, log)
	if err != nil {
		log.Error("file system core could not be created", "err", err)
		e.errorf("olsyn-drive: %v\n", err)
		l.Close()
		return exitError
	}
	opts := agent.Options{
		DriverMinVersion: winfspmsi.MinVersion,
		Version:          version,
		Paths:            e.paths,
		Listener:         l,
		Server:           server,
		DriveFor:         newDriveFor(hc, log),
		FS:               coreFS,
		Mounter:          newMounter(coreFS, log),
		Creds:            creds,
		System:           osSystem{exe: exe, paths: e.paths},
		Logger:           log,
		OnReady: func() {
			cleanupAfterUpdate(e.ctx, log, exe)
		},
		UpdatedFrom: *afterUpdate,
		Licences:    notices.Text(),
	}
	if *rolledBack != "" {
		opts.RolledBack = &agent.Problem{Code: "rolled_back",
			Message: "Version " + *rolledBack + " did not start properly here, so the previous version was put back."}
	}
	updater, store := newUpdater(e, log)
	if updater != nil {
		opts.Updater = updater
	}
	a, err := agent.New(opts)
	if err != nil {
		log.Error("agent could not start", "err", err)
		e.errorf("olsyn-drive: %v\n", err)
		l.Close()
		return exitError
	}
	exit, err := a.Run(e.ctx)
	if err != nil {
		log.Error("agent stopped with an error", "err", err)
	}
	if exit.Update == nil {
		if err != nil {
			return exitError
		}
		return exitOK
	}
	// Hand off to the new version: free the lock and endpoint first.
	release()
	released = true
	if err := handoff(e, log, exe, store, exit.Update); err != nil {
		log.Error("update hand-off failed", "err", err)
		return exitError
	}
	return exitOK
}

// newUpdater returns the update client, or nil when this build has no keys.
func newUpdater(e *env, log *slog.Logger) (*update.Client, *update.Store) {
	store := update.NewStore(filepath.Join(e.paths.State, "update.json"))
	keys, err := update.ReleaseKeys()
	if err != nil || len(keys) == 0 {
		log.Info("updates disabled: no release keys in this build")
		return nil, store
	}
	server, err := currentServer(e)
	if err != nil {
		log.Warn("updates disabled: no server", "err", err)
		return nil, store
	}
	return &update.Client{
		HTTP:       &http.Client{Timeout: 10 * time.Minute},
		BaseURL:    server,
		Keys:       keys,
		Platform:   platform.Name(),
		Current:    version,
		Store:      store,
		StagingDir: filepath.Join(e.paths.State, "updates"),
		Logger:     log,
	}, store
}

// handoff swaps in the staged executable, starts it and waits for it to be
// healthy, rolling back to this version if it is not.
func handoff(e *env, log *slog.Logger, exe string, store *update.Store, p *agent.PendingUpdate) error {
	swap, err := update.Install(exe, p.Staged)
	if err != nil {
		log.Error("could not install the update; restarting this version", "err", err)
		_, serr := platform.StartDetached(exe, []string{"agent"})
		return errors.Join(err, serr)
	}
	client := e.client()
	h := update.Handoff{
		Start: func() (update.Process, error) {
			return platform.StartDetached(exe, []string{"agent", "--after-update", version})
		},
		Healthy: func(ctx context.Context) error {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			st, err := client.State(pctx)
			if err != nil {
				return err
			}
			return healthy(st, p)
		},
		Restart: func() error {
			_, err := platform.StartDetached(exe, []string{"agent", "--rolled-back", p.Version})
			return err
		},
		Store:   store,
		Version: p.Version,
		Logger:  log,
	}
	return h.Run(context.WithoutCancel(e.ctx), swap)
}

// healthy is the update health contract (docs/ipc.md, Updates).
func healthy(st agent.State, p *agent.PendingUpdate) error {
	switch {
	case st.Version != p.Version:
		return fmt.Errorf("agent answers as %s, expected %s", st.Version, p.Version)
	case !st.Ready:
		return errors.New("agent not ready yet")
	case p.WasMounted && st.Mount.State != agent.MountMounted:
		return fmt.Errorf("drive not mounted (%s)", st.Mount.State)
	}
	return nil
}

// cleanupAfterUpdate removes what an update left beside the executable. On
// Windows the previous version may still be running as the hand-off
// supervisor, so it retries for a while.
func cleanupAfterUpdate(ctx context.Context, log *slog.Logger, exe string) {
	for range 12 {
		if err := update.CleanupOld(exe); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	log.Info("old program files still in use; will remove them at the next start")
}

// currentServer reads the configured server URL from the settings file.
func currentServer(e *env) (string, error) {
	data, err := os.ReadFile(filepath.Join(e.paths.Config, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "https://drive.olsyn.com", nil
	}
	if err != nil {
		return "", err
	}
	var s struct {
		ServerURL string `json:"server_url"`
	}
	if err := json.Unmarshal(data, &s); err != nil || s.ServerURL == "" {
		return "https://drive.olsyn.com", nil
	}
	return s.ServerURL, nil
}

// ensureAgent returns a client, starting the agent if its endpoint does not answer.
func ensureAgent(e *env) (*agent.Client, error) {
	c := e.client()
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	_, err := c.State(ctx)
	cancel()
	if err == nil {
		return c, nil
	}
	exe, xerr := os.Executable()
	if xerr != nil {
		return nil, fmt.Errorf("find own executable: %w", xerr)
	}
	if _, err := platform.StartDetached(exe, []string{"agent"}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(e.ctx, time.Second)
		_, err = c.State(ctx)
		cancel()
		if err == nil {
			return c, nil
		}
		select {
		case <-e.ctx.Done():
			return nil, e.ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("the agent did not start: %w", err)
}
