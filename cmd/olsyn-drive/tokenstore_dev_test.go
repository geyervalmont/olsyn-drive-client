//go:build devbuild

package main

import (
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

func TestDevTokenStoreSwitch(t *testing.T) {
	paths := platform.Under(t.TempDir())
	t.Setenv(tokenStoreEnv, "")
	if s, err := devCredentialStore(paths); s != nil || err != nil {
		t.Fatalf("unset: %v %v", s, err)
	}
	t.Setenv(tokenStoreEnv, "file")
	s, err := devCredentialStore(paths)
	if err != nil || s == nil {
		t.Fatalf("file: %v %v", s, err)
	}
	if _, ok := s.(platform.CredentialChecker); !ok {
		t.Fatal("the file store cannot be checked before a sign-in")
	}
	t.Setenv(tokenStoreEnv, "registry")
	if _, err := devCredentialStore(paths); err == nil {
		t.Fatal("an unknown value was accepted")
	}
}
