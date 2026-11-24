//go:build !devbuild

package main

import (
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// Release builds keep the token only in the OS credential store, whatever
// the environment says.
func TestReleaseIgnoresTokenStoreSwitch(t *testing.T) {
	t.Setenv("OLSYN_DRIVE_TOKEN_STORE", "file")
	s, err := devCredentialStore(platform.Under(t.TempDir()))
	if s != nil || err != nil {
		t.Fatalf("release build store = %v, %v", s, err)
	}
}
