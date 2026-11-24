package winfspmsi

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// A build with the installer embedded carries exactly the pinned file.
func TestEmbeddedMatchesPin(t *testing.T) {
	b := Bytes()
	if b == nil {
		t.Skip("built without the winfspmsi tag")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != SHA256 || len(b) != Size {
		t.Fatalf("embedded installer differs from the pinned release")
	}
}
