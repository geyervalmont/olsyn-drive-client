// Package update keeps the client up to date: it fetches the signed release
// manifest for a channel, verifies it with compiled-in Ed25519 keys, refuses
// replays, downloads and checks the platform's update artifact, swaps the
// executable, hands off to the new version, and rolls back if the new version
// does not pass its health check.
package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Product is the manifest's product name for this client.
const Product = "olsyn-drive"

var (
	ErrNoRelease    = errors.New("the channel has no release")
	ErrUnknownKey   = errors.New("manifest signed with an unknown key")
	ErrBadSignature = errors.New("manifest signature is not valid")
	ErrReplayed     = errors.New("manifest is older than one already accepted")
	ErrInvalid      = errors.New("manifest is not valid")
	ErrNoArtifact   = errors.New("release has no update for this platform")
	ErrSizeMismatch = errors.New("downloaded size does not match the manifest")
	ErrHashMismatch = errors.New("downloaded SHA-256 does not match the manifest")
	ErrRolledBack   = errors.New("new version failed its health check and was rolled back")
	ErrNoKeys       = errors.New("no release keys are built into this client")
)

// Envelope is what GET /api/client/v1/releases/{channel}.json returns.
type Envelope struct {
	Channel   string    `json:"channel"`
	Manifest  string    `json:"manifest"` // base64 of the exact signed bytes
	Signature Signature `json:"signature"`
}

// Signature is the detached signature over the decoded manifest bytes.
type Signature struct {
	Alg   string `json:"alg"`
	KeyID string `json:"key_id"`
	Sig   string `json:"sig"` // base64 of 64 bytes
}

// Manifest is the signed release description.
type Manifest struct {
	Schema       int        `json:"schema"`
	Product      string     `json:"product"`
	Channel      string     `json:"channel"`
	Version      string     `json:"version"`
	PublishedAt  time.Time  `json:"published_at"`
	Rollback     bool       `json:"rollback"`
	MinSupported string     `json:"min_supported,omitempty"`
	Driver       *Driver    `json:"driver,omitempty"`
	Notes        string     `json:"notes"`
	Artifacts    []Artifact `json:"artifacts"`
}

// Driver is the file system driver version a release needs.
type Driver struct {
	Name       string `json:"name"`
	MinVersion string `json:"min_version"`
}

// Artifact is one downloadable file of a release.
type Artifact struct {
	Platform string `json:"platform"`
	Role     string `json:"role"` // "installer" or "update"
	File     string `json:"file"`
	URL      string `json:"url"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	MinOS    string `json:"min_os,omitempty"`
}

// Keys maps a key id to its Ed25519 public key.
type Keys map[string]ed25519.PublicKey

var (
	fileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	driverRE   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
)

// Verify checks the envelope's signature over the decoded manifest bytes and
// only then parses those bytes. channel is the channel that was asked for.
func Verify(env Envelope, keys Keys, channel string) (Manifest, error) {
	if len(keys) == 0 {
		return Manifest{}, ErrNoKeys
	}
	if env.Signature.Alg != "ed25519" {
		return Manifest{}, fmt.Errorf("signature algorithm %q: %w", env.Signature.Alg, ErrBadSignature)
	}
	key, ok := keys[env.Signature.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize {
		return Manifest{}, fmt.Errorf("key %q: %w", env.Signature.KeyID, ErrUnknownKey)
	}
	raw, err := base64.StdEncoding.DecodeString(env.Manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", ErrInvalid)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Manifest{}, fmt.Errorf("decode signature: %w", ErrBadSignature)
	}
	if !ed25519.Verify(key, raw, sig) {
		return Manifest{}, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %v: %w", err, ErrInvalid)
	}
	if err := m.validate(channel); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate(channel string) error {
	bad := func(msg string) error { return fmt.Errorf("%s: %w", msg, ErrInvalid) }
	switch {
	case m.Schema != 1:
		return bad(fmt.Sprintf("schema %d not supported", m.Schema))
	case m.Product != Product:
		return bad("manifest is for product " + m.Product)
	case m.Channel != channel:
		return bad(fmt.Sprintf("manifest is for channel %q, asked for %q", m.Channel, channel))
	case m.PublishedAt.IsZero():
		return bad("manifest has no published_at")
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return bad("version: " + err.Error())
	}
	if m.MinSupported != "" {
		if _, err := ParseVersion(m.MinSupported); err != nil {
			return bad("min_supported: " + err.Error())
		}
	}
	if m.Driver != nil {
		if !driverRE.MatchString(m.Driver.Name) {
			return bad("driver name")
		}
		if _, err := parseDotted(m.Driver.MinVersion); err != nil {
			return bad("driver min_version: " + err.Error())
		}
	}
	for _, a := range m.Artifacts {
		if !fileNameRE.MatchString(a.File) || a.Size <= 0 || !sha256RE.MatchString(a.SHA256) || a.URL == "" {
			return bad("artifact " + a.File)
		}
	}
	return nil
}

// UpdateArtifact returns the artifact with role "update" for platform.
func (m Manifest) UpdateArtifact(platform string) (Artifact, error) {
	for _, a := range m.Artifacts {
		if a.Platform == platform && a.Role == "update" {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("%s: %w", platform, ErrNoArtifact)
}
