//go:build devbuild

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// devBuild is set by the devbuild tag: the agent may then use a server other
// than the product's drive (--server, OLSYN_DRIVE_SERVER, settings), plain
// http included. Never shipped.
const devBuild = true

// tokenStoreEnv chooses where a dev build keeps the device token.
// "file" keeps it in an owner-only file under the data folder, for test runs
// driven over SSH where Windows Credential Manager cannot store anything.
const tokenStoreEnv = "OLSYN_DRIVE_TOKEN_STORE"

// devCredentialStore returns the store tokenStoreEnv asks for, or nil for the
// usual one. Release builds have no such switch.
func devCredentialStore(paths platform.Paths) (platform.CredentialStore, error) {
	switch v := os.Getenv(tokenStoreEnv); v {
	case "":
		return nil, nil
	case "file":
		s, err := platform.NewFileCredentialStore(filepath.Join(paths.State, "dev-credentials"))
		if err != nil {
			return nil, fmt.Errorf("%s=file: %w", tokenStoreEnv, err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("%s=%q: the only value is \"file\"", tokenStoreEnv, v)
	}
}
