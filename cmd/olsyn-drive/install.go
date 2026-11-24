package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// cmdInstall copies this executable into the per-user program folder and
// registers start-at-login, the olsyn-drive:// scheme, a Start menu entry
// and the uninstall entry. Nothing here needs administrator rights. Over an
// older installed copy it is the upgrade: the running agent is asked to quit
// (disconnecting the drive), the files are swapped and it starts again.
// Settings, cache and files not yet sent live in the data folder, which is
// left alone.
func cmdInstall(e *env, args []string) int {
	fs := newFlags(e, "install")
	user := fs.Bool("user", false, "install for the current user (the only mode)")
	noAutostart := fs.Bool("no-autostart", false, "do not start the agent at login")
	noStart := fs.Bool("no-start", false, "do not start the agent now")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !*user {
		e.errorf("usage: olsyn-drive install --user\n")
		return exitUsage
	}
	res, err := installSelf(e, installOptions{Autostart: !*noAutostart, Start: !*noStart})
	if err != nil {
		return e.report(err)
	}
	e.printf("Installed Olsyn Drive %s in %s\n", version, filepath.Dir(res.Target))
	for _, n := range res.Notes {
		e.printf("Note: %s\n", n)
	}
	e.printf("Sign in with: %s login\n", quoteArg(res.Target))
	return exitOK
}

type installOptions struct {
	Autostart bool // start at login
	Start     bool // start the installed copy now
}

type installResult struct {
	Target string   // the installed executable
	Notes  []string // registrations this OS does not have
}

// installSelf puts this executable in the install folder (stopping an older
// copy first) and registers it.
func installSelf(e *env, o installOptions) (installResult, error) {
	var res installResult
	dir, err := platform.DefaultInstallDir()
	if err != nil {
		return res, err
	}
	src, err := ownExecutable()
	if err != nil {
		return res, err
	}
	target := filepath.Join(dir, platform.ExecutableName)
	res.Target = target
	if !samePath(src, target) {
		if fileExists(target) {
			if err := stopInstalled(e, target); err != nil {
				return res, err
			}
		}
		if err := installFile(src, target); err != nil {
			return res, err
		}
	}
	// With the desktop app, sign-in starts the tray (which starts the agent
	// when it does not answer); without it, the agent alone.
	startArgs := []string{"agent"}
	if hasUI {
		startArgs = []string{"ui", "--background"}
	}
	notYet := func(what string, err error) error {
		if errors.Is(err, platform.ErrNotImplemented) {
			res.Notes = append(res.Notes, what+" is not available on this system yet")
			return nil
		}
		if err != nil {
			return fmt.Errorf("register %s: %w", what, err)
		}
		return nil
	}
	if o.Autostart {
		if err := notYet("start at login", platform.EnableAutostart(target, startArgs)); err != nil {
			return res, err
		}
	}
	if err := notYet("olsyn-drive:// links", platform.RegisterURLScheme(target)); err != nil {
		return res, err
	}
	if err := notYet("the Start menu entry", platform.CreateStartMenuShortcut(target)); err != nil {
		// A missing shortcut does not stop the program from working.
		e.errorf("olsyn-drive: %v\n", err)
	}
	size := uint32(0)
	if fi, err := os.Stat(target); err == nil {
		size = uint32(fi.Size() / 1024)
	}
	err = platform.RegisterUninstall(platform.UninstallEntry{
		DisplayName:      platform.ProductName,
		DisplayVersion:   version,
		Publisher:        "Olsyn",
		InstallLocation:  dir,
		UninstallCommand: quoteArg(target) + " uninstall --interactive",
		DisplayIcon:      target,
		EstimatedSizeKB:  size,
	})
	if err := notYet("the uninstall entry", err); err != nil {
		return res, err
	}
	if o.Start {
		now := []string{"agent"}
		if hasUI {
			now = nil // the app opens (on its first-run screen) and starts the agent
		}
		if _, err := platform.StartDetached(target, now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// stopInstalled stops the installed copy before it is replaced: the agent
// is asked to quit cleanly (it disconnects the drive and keeps its queue),
// then whatever still runs from that file (the tray app) is ended.
func stopInstalled(e *env, target string) error {
	if err := stopAgent(e); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	if err := platform.StopProgram(ctx, target); err != nil && !errors.Is(err, platform.ErrNotImplemented) {
		return fmt.Errorf("stop the running Olsyn Drive: %w", err)
	}
	return nil
}

func ownExecutable() (string, error) {
	p, err := os.Executable()
	if err == nil {
		p, err = filepath.EvalSymlinks(p)
	}
	if err != nil {
		return "", fmt.Errorf("find own executable: %w", err)
	}
	return p, nil
}

// cmdUninstall stops the agent, removes the registrations and the program
// folder, and deletes settings, cache and logs unless --keep-cache is given.
// Files that have not reached the drive are never deleted without --yes (or,
// with --interactive, without the person agreeing in a dialog).
func cmdUninstall(e *env, args []string) int {
	fs := newFlags(e, "uninstall")
	keep := fs.Bool("keep-cache", false, "keep settings and the cache (files not yet sent go on after a reinstall)")
	yes := fs.Bool("yes", false, "delete files that have not reached the drive")
	interactive := fs.Bool("interactive", false, "ask in dialogs (Settings > Apps runs this)")
	fs.Bool("purge", true, "accepted for older scripts; deleting the data is the default")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *interactive {
		if !askOKCancel("Uninstall Olsyn Drive", "Remove Olsyn Drive from this PC?\n\nYour files on the drive are not affected.") {
			return exitOK
		}
		*keep = askYesNo("Uninstall Olsyn Drive", "Keep the copies of drive files on this PC, for a later reinstall?\n\nChoose No to delete them; they stay on the drive.", false)
	}
	if !*keep {
		err := signOutClearing(e, *yes)
		var ae *agent.APIError
		if errors.As(err, &ae) && ae.Code == "unsent_data" {
			if !*interactive {
				e.errorf("olsyn-drive: %s\nRun again with --yes to delete them, or with --keep-cache to keep them.\n", ae.Message)
				return exitNeedsInput
			}
			if askYesNo("Uninstall Olsyn Drive", ae.Message+"\n\nDelete them anyway? Choose No to keep them on this PC; they are sent when Olsyn Drive is installed again.", false) {
				err = signOutClearing(e, true)
			} else {
				*keep, err = true, nil
			}
		}
		if err != nil && !isNoAgent(err) {
			e.errorf("olsyn-drive: sign-out: %v\n", err)
		}
	}
	var errs []error
	if dir, err := platform.DefaultInstallDir(); err == nil {
		if err := stopInstalled(e, filepath.Join(dir, platform.ExecutableName)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, step := range []func() error{platform.DisableAutostart, platform.UnregisterURLScheme, platform.RemoveStartMenuShortcut, platform.UnregisterUninstall} {
		if err := step(); err != nil && !errors.Is(err, platform.ErrNotImplemented) {
			errs = append(errs, err)
		}
	}
	if !*keep {
		for _, d := range []string{e.paths.Cache, e.paths.Logs, e.paths.State, e.paths.Config} {
			if d != "" {
				if err := os.RemoveAll(d); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	if dir, err := platform.DefaultInstallDir(); err == nil && fileExists(dir) {
		if err := platform.RemoveAfterExit(dir); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		if *interactive {
			showError("Olsyn Drive was not fully removed", err.Error())
		}
		return e.report(err)
	}
	msg := "Olsyn Drive was removed."
	if *keep {
		msg += " Settings and cached files were kept."
	}
	if *interactive {
		showInfo("Uninstall Olsyn Drive", msg)
	}
	e.printf("%s\n", msg)
	return exitOK
}

// signOutClearing signs this computer out and clears the cache. Without
// discardUnsent the agent refuses (409 unsent_data) while files have not
// reached the drive.
func signOutClearing(e *env, discardUnsent bool) error {
	body := map[string]bool{"clear_cache": true, "discard_unsent_confirmed": discardUnsent}
	return e.client().Do(e.ctx, http.MethodPost, "/v1/signout", body, nil)
}

// stopAgent asks a running agent to quit (disconnecting the drive) and waits
// until its endpoint stops answering. No agent running is not an error; one
// that still answers after 30 s is.
func stopAgent(e *env) error {
	c := e.client()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	if err := c.Do(ctx, http.MethodPost, "/v1/quit", map[string]bool{"unmount": true}, nil); err != nil {
		return nil
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(e.ctx, time.Second)
		_, err := c.State(pctx)
		pcancel()
		if err != nil {
			return nil
		}
		select {
		case <-e.ctx.Done():
			return e.ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return errors.New("the running Olsyn Drive did not stop; quit it from its tray icon and try again")
}

// installFile copies src to dst through a temporary file and a rename. A
// target that is in use is moved aside first (allowed on Windows).
func installFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	tmp := dst + ".installing"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy to %s: %w", tmp, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		old := dst + ".old"
		os.Remove(old)
		if rerr := os.Rename(dst, old); rerr != nil {
			os.Remove(tmp)
			return fmt.Errorf("replace %s: %w", dst, err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			os.Rename(old, dst)
			os.Remove(tmp)
			return fmt.Errorf("replace %s: %w", dst, err)
		}
	}
	return nil
}

func samePath(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// quoteArg quotes a path for a command line shown to people or stored in the registry.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}
