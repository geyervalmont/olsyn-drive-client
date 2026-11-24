package main

import (
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/update"
)

// errUnverified refuses an install whose manifest this build cannot check.
var errUnverified = errors.New("this build has no release keys, so it cannot check the release manifest's signature (development builds only); add --allow-unverified to install it anyway")

// checkInstallManifest is "install --manifest": the install script saved the
// drive's release answer without checking its Ed25519 signature (Windows
// PowerShell 5.1 cannot); this program checks it with its compiled-in keys
// and requires that it is the release's installer for this platform. Any
// failure refuses the install.
func checkInstallManifest(e *env, path string, allowUnverified bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the release manifest: %w", err)
	}
	self, err := ownExecutable()
	if err != nil {
		return err
	}
	keys, err := installKeys()
	if err != nil {
		return err
	}
	m, err := verifyInstallManifest(raw, keys, platform.Name(), self, allowUnverified)
	switch {
	case errors.Is(err, update.ErrNoKeys) && allowUnverified:
		e.errorf("Warning: this build has no release keys, so the release manifest's signature was NOT checked. Development builds only.\n")
		return nil
	case err != nil:
		return err
	}
	e.printf("Release manifest verified: Olsyn Drive %s (%s), signed, and this file is its installer.\n", m.Version, m.Channel)
	return nil
}

// verifyInstallManifest checks a release answer against keys and the
// installer file self.
func verifyInstallManifest(raw []byte, keys update.Keys, platformName, self string, allowUnverified bool) (update.Manifest, error) {
	m, _, err := update.VerifyInstaller(raw, keys, platformName, self)
	switch {
	case errors.Is(err, update.ErrNoKeys):
		if allowUnverified {
			return m, err
		}
		return m, errUnverified
	case err != nil:
		return m, fmt.Errorf("the release manifest does not check out, so nothing was installed: %w", err)
	}
	return m, nil
}

// installKeys are the compiled-in release keys plus, in dev builds only,
// the ones devReleaseKeysEnv names.
func installKeys() (update.Keys, error) {
	keys, err := update.ReleaseKeys()
	if err != nil {
		return nil, err
	}
	extra, err := devReleaseKeys()
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		parsed, err := update.ParseKeys(extra)
		if err != nil {
			return nil, err
		}
		keys = maps.Clone(keys)
		if keys == nil {
			keys = update.Keys{}
		}
		maps.Copy(keys, parsed)
	}
	return keys, nil
}
