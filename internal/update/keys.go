package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// releaseKeys lists the public keys release manifests may be signed with,
// key id -> base64 of the 32-byte Ed25519 public key. The private keys never
// live in this repository. Add the production key here before the first
// release; with none, every update check fails with ErrNoKeys.
func releaseKeys() map[string]string {
	return map[string]string{}
}

// ReleaseKeys returns the compiled-in public keys.
func ReleaseKeys() (Keys, error) {
	return ParseKeys(releaseKeys())
}

// ParseKeys decodes key id -> base64 public key pairs.
func ParseKeys(in map[string]string) (Keys, error) {
	keys := Keys{}
	for id, b64 := range in {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release key %q is not a base64 Ed25519 public key", id)
		}
		keys[id] = ed25519.PublicKey(raw)
	}
	return keys, nil
}
