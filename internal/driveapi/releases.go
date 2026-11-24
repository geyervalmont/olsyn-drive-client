package driveapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

// SignedRelease is a channel's release manifest as served: the exact signed
// bytes and their signature. Verify Signature over Manifest with the
// compiled-in public key, then parse ONLY Manifest (internal/update does).
type SignedRelease struct {
	Channel   string
	Manifest  []byte
	Algorithm string
	KeyID     string
	Signature []byte
	ETag      string
}

// Release reads GET /api/client/v1/releases/{channel}.json (public, no auth).
// With ifNoneMatch set and unchanged, it returns (nil, nil).
func (c *Client) Release(ctx context.Context, channel, ifNoneMatch string) (*SignedRelease, error) {
	h := http.Header{}
	if ifNoneMatch != "" {
		h.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := c.send(ctx, request{op: "release manifest", method: http.MethodGet,
		path: pathID("/api/client/v1/releases/", channel, ".json"), header: h, noAuth: true,
		accept2: []int{http.StatusNotModified}})
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil
	}
	var raw struct {
		Channel   string `json:"channel"`
		Manifest  string `json:"manifest"`
		Signature struct {
			Alg   string `json:"alg"`
			KeyID string `json:"key_id"`
			Sig   string `json:"sig"`
		} `json:"signature"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("drive: release manifest: decode: %w", err)
	}
	man, err := base64.StdEncoding.DecodeString(raw.Manifest)
	if err != nil {
		return nil, fmt.Errorf("drive: release manifest: manifest is not base64: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(raw.Signature.Sig)
	if err != nil {
		return nil, fmt.Errorf("drive: release manifest: signature is not base64: %w", err)
	}
	return &SignedRelease{Channel: raw.Channel, Manifest: man, Algorithm: raw.Signature.Alg,
		KeyID: raw.Signature.KeyID, Signature: sig, ETag: resp.Header.Get("ETag")}, nil
}

// ArtifactURL is the absolute URL of a manifest artifact's relative url.
func (c *Client) ArtifactURL(rel string) string { return c.base.String() + rel }
