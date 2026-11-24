package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// launchAction is what a double-clicked copy of the program does.
type launchAction int

const (
	launchHere      launchAction = iota // run the desktop app from this file
	launchInstall                       // nothing installed: offer to install
	launchUpgrade                       // an older (or different dev) build is installed: offer to replace it
	launchInstalled                     // the same or a newer version is installed: open that one
)

// planLaunch decides between installing, upgrading and opening the
// installed copy. Release versions are compared; a dev build (or one whose
// version cannot be read) replaces the installed copy when the files differ.
func planLaunch(isInstalledCopy, installed bool, self, installedVersion string, sameFile func() bool) launchAction {
	switch {
	case isInstalledCopy:
		return launchHere
	case !installed:
		return launchInstall
	}
	if !isDevVersion(self) && !isDevVersion(installedVersion) {
		if c, err := update.CompareStrings(self, installedVersion); err == nil {
			if c > 0 {
				return launchUpgrade
			}
			return launchInstalled
		}
	}
	if sameFile() {
		return launchInstalled
	}
	return launchUpgrade
}

func isDevVersion(v string) bool {
	if v == "" || strings.Contains(v, "dev") {
		return true
	}
	_, err := update.ParseVersion(v)
	return err != nil
}

// offerInstall runs before the desktop app when the program was opened with
// no arguments (a double-click, typically in Downloads). It returns done
// when this process should exit instead of showing the app here.
func offerInstall(e *env) (code int, done bool) {
	if runtime.GOOS != "windows" || !hasUI {
		return 0, false
	}
	return firstLaunch(e, osLaunchDeps(e))
}

// launchDeps are what firstLaunch needs from the system; a test replaces
// them to check that nothing is written before the person agrees.
type launchDeps struct {
	installDir       func() (string, error)
	self             func() (string, error)
	installedVersion func() (string, error)
	ask              func(title, text string) bool
	showError        func(title, text string)
	start            func(exe string) error
	install          func() error
}

func osLaunchDeps(e *env) launchDeps {
	return launchDeps{
		installDir:       platform.DefaultInstallDir,
		self:             ownExecutable,
		installedVersion: platform.InstalledVersion,
		ask:              askOKCancel,
		showError:        showError,
		start: func(exe string) error {
			_, err := platform.StartDetached(exe, nil)
			return err
		},
		install: func() error {
			_, err := installSelf(e, installOptions{Autostart: true, Start: true})
			return err
		},
	}
}

// firstLaunch decides between installing, updating and opening the
// installed copy, and asks first. Until the person agrees it only reads:
// no folder, log, setting or registration is written, so a copy opened from
// Downloads and declined leaves nothing behind.
func firstLaunch(e *env, d launchDeps) (code int, done bool) {
	dir, err := d.installDir()
	if err == nil {
		var self string
		if self, err = d.self(); err == nil {
			return launchFrom(e, d, self, filepath.Join(dir, platform.ExecutableName))
		}
	}
	// Running the app from here would write its logs and window state
	// unasked: say what is wrong instead.
	d.showError("Olsyn Drive cannot be installed", err.Error())
	return e.report(err), true
}

func launchFrom(e *env, d launchDeps, self, target string) (int, bool) {
	installedVersion, _ := d.installedVersion()
	plan := planLaunch(samePath(self, target), fileExists(target), version, installedVersion,
		func() bool { return sameContent(self, target) })
	switch plan {
	case launchHere:
		return 0, false
	case launchInstalled:
		if err := d.start(target); err != nil {
			return e.report(err), true
		}
		return exitOK, true
	case launchInstall:
		if !d.ask(installQuestion(dataPresent(e.paths))) {
			return exitOK, true
		}
	case launchUpgrade:
		from := installedVersion
		if from == "" {
			from = "the installed version"
		}
		if !d.ask("Update Olsyn Drive", fmt.Sprintf("Update Olsyn Drive from %s to %s?\n\n"+
			"The drive disconnects for a moment. Files waiting to upload are kept and carry on after the update.", from, version)) {
			return exitOK, true
		}
	}
	if err := d.install(); err != nil {
		d.showError("Olsyn Drive could not be installed", err.Error())
		return e.report(err), true
	}
	return exitOK, true
}

// installQuestion is the question a copy opened with nothing installed asks.
// With the data folder still there (the program was removed, by the person
// or by security software) it is a reinstall that keeps the waiting files.
func installQuestion(reinstall bool) (title, text string) {
	const how = "It installs for you only, needs no administrator rights, and starts when you sign in to Windows. " +
		"You can remove it in Settings > Apps."
	if reinstall {
		return "Reinstall Olsyn Drive", "Reinstall Olsyn Drive on this PC?\n\n" +
			"Olsyn Drive is no longer installed here, but its data is: reinstalling keeps your waiting files, " +
			"and anything not yet uploaded carries on once it is installed again. " +
			"If Windows Security removed it, see " + blocked.HelpURL + " first.\n\n" + how
	}
	return "Install Olsyn Drive", "Install Olsyn Drive on this PC?\n\n" + how
}

// dataPresent reports whether this user's data folder (settings, queue,
// cache) exists. It only looks.
func dataPresent(p platform.Paths) bool {
	for _, d := range []string{p.Config, p.State} {
		if d != "" && fileExists(d) {
			return true
		}
	}
	return false
}

// sameContent reports whether two files hold the same bytes.
func sameContent(a, b string) bool {
	ha, err1 := fileSHA256(a)
	hb, err2 := fileSHA256(b)
	return err1 == nil && err2 == nil && bytes.Equal(ha, hb)
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
