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
	dir, err := platform.DefaultInstallDir()
	if err != nil {
		return 0, false
	}
	self, err := ownExecutable()
	if err != nil {
		return 0, false
	}
	target := filepath.Join(dir, platform.ExecutableName)
	installedVersion, _ := platform.InstalledVersion()
	plan := planLaunch(samePath(self, target), fileExists(target), version, installedVersion,
		func() bool { return sameContent(self, target) })
	switch plan {
	case launchHere:
		return 0, false
	case launchInstalled:
		if _, err := platform.StartDetached(target, nil); err != nil {
			return e.report(err), true
		}
		return exitOK, true
	case launchInstall:
		if !askOKCancel("Install Olsyn Drive", "Install Olsyn Drive on this PC?\n\n"+
			"It installs for you only, needs no administrator rights, and starts when you sign in to Windows. "+
			"You can remove it in Settings > Apps.") {
			return exitOK, true
		}
	case launchUpgrade:
		from := installedVersion
		if from == "" {
			from = "the installed version"
		}
		if !askOKCancel("Update Olsyn Drive", fmt.Sprintf("Update Olsyn Drive from %s to %s?\n\n"+
			"The drive disconnects for a moment. Files waiting to upload are kept and carry on after the update.", from, version)) {
			return exitOK, true
		}
	}
	if _, err := installSelf(e, installOptions{Autostart: true, Start: true}); err != nil {
		showError("Olsyn Drive could not be installed", err.Error())
		return e.report(err), true
	}
	return exitOK, true
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
