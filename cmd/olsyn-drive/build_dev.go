//go:build devbuild

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// devReleaseKeysEnv lets a dev build trust more release keys
// ("id=base64,id2=base64"), so that "install --manifest" can be tried
// against cmd/fakedrive-server, which signs with a key it makes at start.
const devReleaseKeysEnv = "OLSYN_DRIVE_DEV_RELEASE_KEYS"

// devReleaseKeys reads devReleaseKeysEnv. Release builds have no such switch.
func devReleaseKeys() (map[string]string, error) {
	v := strings.TrimSpace(os.Getenv(devReleaseKeysEnv))
	if v == "" {
		return nil, nil
	}
	keys := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		id, key, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || id == "" || key == "" {
			return nil, fmt.Errorf("%s: want id=base64[,id=base64]", devReleaseKeysEnv)
		}
		keys[id] = key
	}
	return keys, nil
}

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
