//go:build !devbuild

package main

import "github.com/geyervalmont/olsyn-drive-client/internal/platform"

// devBuild is false in release builds: they talk only to the product's drive.
const devBuild = false

// devReleaseKeys is always empty in release builds: only compiled-in keys count.
func devReleaseKeys() (map[string]string, error) { return nil, nil }

// devCredentialStore is always nil in release builds: the token lives only
// in the OS credential store.
func devCredentialStore(platform.Paths) (platform.CredentialStore, error) { return nil, nil }
