package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
)

// Client checks for and downloads releases from one drive server.
type Client struct {
	HTTP       *http.Client
	BaseURL    string // drive server, e.g. https://drive.olsyn.com
	Keys       Keys
	Platform   string // release platform name, e.g. windows-x64
	Current    string // running version
	Store      *Store
	StagingDir string
	Logger     *slog.Logger
}

// Release is the outcome of a check.
type Release struct {
	Manifest Manifest
	// Artifact is this platform's update file; nil when the release has none.
	Artifact *Artifact
	// Newer: applying it changes the running version (a higher version, or a
	// signed rollback to another one).
	Newer bool
	// Required: the running version is below the release's min_supported.
	Required bool
	// PreviouslyFailed holds the reason when this version already failed its health check here.
	PreviouslyFailed string
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Check fetches and verifies the channel's manifest. ErrNoRelease means the
// channel has none (not an error for the person).
func (c *Client) Check(ctx context.Context, channel string) (*Release, error) {
	u, err := c.resolve("/api/client/v1/releases/" + url.PathEscape(channel) + ".json")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release manifest: HTTP %d", resp.StatusCode)
	}
	var env Envelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("parse release envelope: %w", err)
	}
	m, err := Verify(env, c.Keys, channel)
	if err != nil {
		return nil, err
	}
	if err := c.Store.Accept(m); err != nil {
		return nil, err
	}
	rel := &Release{Manifest: m}
	if a, err := m.UpdateArtifact(c.Platform); err == nil {
		rel.Artifact = &a
	}
	cmp, err := CompareStrings(m.Version, c.Current)
	if err != nil {
		// A development build ("dev") is never offered updates automatically.
		c.logger().Info("running version is not semantic; no update offered", "current", c.Current)
		return rel, nil
	}
	rel.Newer = cmp > 0 || (m.Rollback && cmp != 0)
	if m.MinSupported != "" {
		if c2, err := CompareStrings(c.Current, m.MinSupported); err == nil && c2 < 0 {
			rel.Required = true
		}
	}
	if reason, ok := c.Store.Failed(m.Version); ok {
		rel.PreviouslyFailed = reason
	}
	c.logger().Info("release checked", "channel", channel, "version", m.Version, "newer", rel.Newer, "required", rel.Required)
	return rel, nil
}

// Download fetches the release's update artifact into the staging folder,
// checking size and SHA-256, and returns the staged path. progress may be nil.
func (c *Client) Download(ctx context.Context, rel *Release, progress func(done, total int64)) (string, error) {
	if rel == nil || rel.Artifact == nil {
		return "", ErrNoArtifact
	}
	a := *rel.Artifact
	u, err := c.resolve(a.URL)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(c.StagingDir, rel.Manifest.Version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create staging folder: %w", err)
	}
	final := filepath.Join(dir, a.File)
	if ok, _ := fileMatches(final, a.Size, a.SHA256); ok {
		return final, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download update: HTTP %d", resp.StatusCode)
	}
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", part, err)
	}
	h := sha256.New()
	w := &progressWriter{w: io.MultiWriter(f, h), total: a.Size, fn: progress}
	n, err := io.Copy(w, io.LimitReader(resp.Body, a.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return "", fmt.Errorf("download update: %w", err)
	}
	if n != a.Size {
		os.Remove(part)
		return "", fmt.Errorf("got %d bytes, manifest says %d: %w", n, a.Size, ErrSizeMismatch)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		os.Remove(part)
		return "", ErrHashMismatch
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", fmt.Errorf("stage update: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(final, 0o755); err != nil {
			return "", fmt.Errorf("make update executable: %w", err)
		}
	}
	c.logger().Info("update staged", "version", rel.Manifest.Version, "bytes", n)
	return final, nil
}

// resolve turns a server-relative path into an absolute URL on the server.
func (c *Client) resolve(ref string) (string, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parse server url: %w", err)
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", ref, err)
	}
	return base.ResolveReference(r).String(), nil
}

func fileMatches(path string, size int64, sum string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() != size {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sum, nil
}

type progressWriter struct {
	w     io.Writer
	done  int64
	total int64
	fn    func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.fn != nil {
		p.fn(p.done, p.total)
	}
	return n, err
}
