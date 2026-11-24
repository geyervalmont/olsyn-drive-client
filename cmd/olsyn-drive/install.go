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

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// cmdInstall copies this executable into the per-user program folder and
// registers start-at-login, the olsyn-drive:// scheme and the uninstall entry.
// Nothing here needs administrator rights.
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
	dir, err := platform.DefaultInstallDir()
	if err != nil {
		return e.report(err)
	}
	src, err := os.Executable()
	if err == nil {
		src, err = filepath.EvalSymlinks(src)
	}
	if err != nil {
		return e.report(fmt.Errorf("find own executable: %w", err))
	}
	target := filepath.Join(dir, platform.ExecutableName)
	if !samePath(src, target) {
		stopAgent(e) // a running agent may hold the old executable
		if err := installFile(src, target); err != nil {
			return e.report(err)
		}
		// The WinFsp installer travels next to the executable when bundled.
		if msi := filepath.Join(filepath.Dir(src), "winfsp.msi"); fileExists(msi) {
			if err := installFile(msi, filepath.Join(dir, "winfsp.msi")); err != nil {
				return e.report(err)
			}
		}
	}
	var notes []string
	// With the desktop app, sign-in starts the tray (which starts the agent
	// when it does not answer); without it, the agent alone.
	startArgs := []string{"agent"}
	if hasUI {
		startArgs = []string{"ui", "--background"}
	}
	if !*noAutostart {
		if err := platform.EnableAutostart(target, startArgs); err != nil {
			return e.report(fmt.Errorf("register start at login: %w", err))
		}
	}
	if err := platform.RegisterURLScheme(target); err != nil {
		if !errors.Is(err, platform.ErrNotImplemented) {
			return e.report(fmt.Errorf("register olsyn-drive:// links: %w", err))
		}
		notes = append(notes, "olsyn-drive:// links are not registered on this system yet")
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
		UninstallCommand: quoteArg(target) + " uninstall",
		DisplayIcon:      target,
		EstimatedSizeKB:  size,
	})
	if err != nil && !errors.Is(err, platform.ErrNotImplemented) {
		return e.report(fmt.Errorf("register uninstall entry: %w", err))
	}
	if !*noStart {
		now := []string{"agent"}
		if hasUI {
			now = nil // the app opens on its first-run screen and starts the agent
		}
		if _, err := platform.StartDetached(target, now); err != nil {
			return e.report(err)
		}
	}
	e.printf("Installed Olsyn Drive %s in %s\n", version, dir)
	for _, n := range notes {
		e.printf("Note: %s\n", n)
	}
	e.printf("Sign in with: %s login\n", quoteArg(target))
	return exitOK
}

// cmdUninstall stops the agent, removes the registrations and the program
// folder. With --purge it also signs out and deletes settings, cache and logs.
func cmdUninstall(e *env, args []string) int {
	fs := newFlags(e, "uninstall")
	purge := fs.Bool("purge", false, "also sign out and delete settings, cache and logs")
	yes := fs.Bool("yes", false, "with --purge, confirm deleting files that have not reached the drive")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *purge {
		c := e.client()
		body := map[string]bool{"clear_cache": true, "discard_unsent_confirmed": *yes}
		if err := c.Do(e.ctx, http.MethodPost, "/v1/signout", body, nil); err != nil && !isNoAgent(err) {
			if strings.Contains(err.Error(), "not reached the drive") {
				e.errorf("%v\nRun again with --yes to delete them anyway.\n", err)
				return exitNeedsInput
			}
			e.errorf("olsyn-drive: sign-out: %v\n", err)
		}
	}
	stopAgent(e)
	var errs []error
	for _, step := range []func() error{platform.DisableAutostart, platform.UnregisterURLScheme, platform.UnregisterUninstall} {
		if err := step(); err != nil && !errors.Is(err, platform.ErrNotImplemented) {
			errs = append(errs, err)
		}
	}
	if *purge {
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
		return e.report(err)
	}
	e.printf("Olsyn Drive was removed.\n")
	if !*purge {
		e.printf("Settings and cache were kept; \"uninstall --purge\" removes them too.\n")
	}
	return exitOK
}

// stopAgent asks a running agent to quit (disconnecting the drive) and waits
// until its endpoint stops answering.
func stopAgent(e *env) {
	c := e.client()
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	if err := c.Do(ctx, http.MethodPost, "/v1/quit", map[string]bool{"unmount": true}, nil); err != nil {
		return
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pctx, pcancel := context.WithTimeout(e.ctx, time.Second)
		_, err := c.State(pctx)
		pcancel()
		if err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
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
