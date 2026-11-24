// Package driveapi is the drive's HTTP API as the desktop client uses it:
// uploads (start, parts, complete), the pre-check, and the relay routes
// (the drive's documented upload and relay routes). It holds the token and never sends it
// anywhere but the drive's own origin: presigned storage URLs get no
// Authorization header.
package driveapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one drive.
type Client struct {
	Base  *url.URL // https://drive.example (no trailing slash)
	token string
	HTTP  *http.Client // drive API calls
	Store *http.Client // presigned storage PUTs (no auth header ever)
	Agent string
}

// New makes a client. base must be https, or http on a loopback host
// (development and tests).
func New(base, token, agent string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("drive address %q is not a URL", base)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("drive address must be https (http only for a loopback test drive)")
	}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 2 * time.Minute}
	return &Client{Base: u, token: token, Agent: agent,
		HTTP:  &http.Client{Transport: tr, Timeout: 2 * time.Minute, CheckRedirect: noRedirect},
		Store: &http.Client{Transport: tr.Clone(), CheckRedirect: noRedirect}}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// TrustCA adds the certificates in a PEM file to the ones trusted for the
// drive (a drive behind a private certificate authority).
func (c *Client) TrustCA(pemFile string) error {
	b, err := os.ReadFile(pemFile)
	if err != nil {
		return err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(b) {
		return fmt.Errorf("%s has no PEM certificates", pemFile)
	}
	for _, hc := range []*http.Client{c.HTTP, c.Store} {
		if tr, ok := hc.Transport.(*http.Transport); ok {
			tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
	}
	return nil
}

// SetTransport replaces both transports (tests).
func (c *Client) SetTransport(rt http.RoundTripper) {
	c.HTTP.Transport, c.Store.Transport = rt, rt
}

// Error is a refusal or failure from the drive or storage.
type Error struct {
	Status int
	Code   string
	Msg    string
	Retry  time.Duration // Retry-After, when sent
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("%d %s", e.Status, e.Code)
}

// Retryable says whether trying again later can succeed: the network, a
// drive deploy (503), overload (429) or a server error. A 4xx refusal is
// final for that request.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status == 0 || e.Status == 408 || e.Status == 425 || e.Status == 429 || e.Status >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true // transport errors: connection refused, reset, timeouts
}

// StatusOf is the HTTP status of an API error (0 when it isn't one).
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := *c.Base
	p, q, _ := strings.Cut(path, "?")
	u.Path = strings.TrimRight(c.Base.Path, "/") + p
	u.RawQuery = q
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.Agent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode}
		var m struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &m) == nil {
			e.Code, e.Msg = m.Error, m.Message
		}
		if s := resp.Header.Get("Retry-After"); s != "" {
			var n int
			if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 && n < 3600 {
				e.Retry = time.Duration(n) * time.Second
			}
		}
		return e
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// ---- Uploads ----

// UploadIn starts (or resumes, by fingerprint) an upload.
type UploadIn struct {
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Mime        string `json:"mime"`
	Conflict    string `json:"conflict,omitempty"` // fail | rename | new_version
	Fingerprint string `json:"fingerprint,omitempty"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty"`
	Assurance   string `json:"assurance,omitempty"`
}

type Part struct {
	Number int32  `json:"number"`
	ETag   string `json:"etag,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

type PartURL struct {
	Number int32  `json:"number"`
	URL    string `json:"url"`
}

type Ticket struct {
	UploadID  string            `json:"upload_id"`
	NodeID    string            `json:"node_id"`
	VersionID string            `json:"version_id"`
	Method    string            `json:"method"` // single | multipart
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	PartSize  int64             `json:"part_size"`
	PartCount int32             `json:"part_count"`
	Parts     []PartURL         `json:"parts"`
	Uploaded  []Part            `json:"uploaded"`
	Resumed   bool              `json:"resumed"`
	Name      string            `json:"name"`
}

func (c *Client) StartUpload(ctx context.Context, in UploadIn) (*Ticket, error) {
	var t Ticket
	return &t, c.do(ctx, "POST", "/api/v1/uploads", in, &t)
}

func (c *Client) PartURLs(ctx context.Context, uploadID string, numbers []int32) ([]PartURL, error) {
	var out struct {
		Parts []PartURL `json:"parts"`
	}
	err := c.do(ctx, "POST", "/api/v1/uploads/"+url.PathEscape(uploadID)+"/parts", map[string]any{"numbers": numbers}, &out)
	return out.Parts, err
}

// Completed is where a completed upload landed.
type Completed struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
}

func (c *Client) Complete(ctx context.Context, uploadID string, parts []Part) (*Completed, error) {
	var out Completed
	return &out, c.do(ctx, "POST", "/api/v1/uploads/"+url.PathEscape(uploadID)+"/complete", map[string]any{"parts": parts}, &out)
}

func (c *Client) Abort(ctx context.Context, uploadID string) error {
	return c.do(ctx, "DELETE", "/api/v1/uploads/"+url.PathEscape(uploadID), nil, nil)
}

// CheckIn / CheckOut: POST /api/v1/uploads/check.
type CheckIn struct {
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Fingerprint string `json:"fingerprint,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type CheckOut struct {
	State    string `json:"state"` // present | different | absent
	ID       string `json:"id"`
	SameSize bool   `json:"same_size"`
	SHA256   string `json:"sha256"`
}

// MaxCheck is the most files one check answers for.
const MaxCheck = 1000

func (c *Client) Check(ctx context.Context, person, assurance string, files []CheckIn) ([]CheckOut, error) {
	var out struct {
		Items []CheckOut `json:"items"`
	}
	body := map[string]any{"files": files}
	if person != "" {
		body["on_behalf_of"] = person
	}
	if assurance != "" {
		body["assurance"] = assurance
	}
	if err := c.do(ctx, "POST", "/api/v1/uploads/check", body, &out); err != nil {
		return nil, err
	}
	if len(out.Items) != len(files) {
		return nil, fmt.Errorf("check answered %d of %d files", len(out.Items), len(files))
	}
	return out.Items, nil
}

// ---- Storage ----

// Put sends one body to a presigned URL and returns storage's ETag. 412
// (If-None-Match on a single PUT whose object exists) comes back as an
// *Error with Status 412.
func (c *Client) Put(ctx context.Context, rawURL string, headers map[string]string, body io.Reader, size int64) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (isLoopback(u.Hostname()) || u.Host == c.Base.Host))) {
		return "", fmt.Errorf("refusing a storage URL that isn't https")
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", rawURL, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.Store.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		code := "storage_error"
		if i := bytes.Index(b, []byte("<Code>")); i >= 0 {
			if j := bytes.Index(b[i:], []byte("</Code>")); j > 6 {
				code = string(b[i+6 : i+j])
			}
		}
		return "", &Error{Status: resp.StatusCode, Code: code}
	}
	return resp.Header.Get("ETag"), nil
}

// ---- Relay routes ----

type UserRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Drop struct {
	ID           string  `json:"id"`
	Person       UserRef `json:"person"`
	FolderID     string  `json:"folder_id"`
	FolderName   string  `json:"folder_name"`
	Label        string  `json:"label"`
	OnConflict   string  `json:"on_conflict"`
	PersonFolder string  `json:"person_folder"`
	Path         string  `json:"path"`
}

type Self struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Mode         string   `json:"mode"`
	Person       *UserRef `json:"person"`
	SharePrefix  string   `json:"share_prefix"`
	MaxAssurance string   `json:"max_assurance"`
	Drops        []Drop   `json:"drops"`
}

func (c *Client) Self(ctx context.Context) (*Self, error) {
	var s Self
	return &s, c.do(ctx, "GET", "/api/relay/v1/self", nil, &s)
}

type Heartbeat struct {
	Version    string `json:"version"`
	OS         string `json:"os"`
	Machine    string `json:"machine"`
	FreeBytes  *int64 `json:"free_bytes,omitempty"`
	SpoolBytes *int64 `json:"spool_bytes,omitempty"`
	Queued     int64  `json:"queued"`
	Sending    int64  `json:"sending"`
	Held       int64  `json:"held"`
	Failed     int64  `json:"failed"`
	Full       bool   `json:"full"`
}

func (c *Client) Heartbeat(ctx context.Context, hb Heartbeat) error {
	return c.do(ctx, "POST", "/api/relay/v1/heartbeat", hb, nil)
}

type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Folder checks the person (and node) may deliver into folder id.
func (c *Client) Folder(ctx context.Context, id, person string) (*Folder, error) {
	q := ""
	if person != "" {
		q = "?on_behalf_of=" + url.QueryEscape(person)
	}
	var f Folder
	return &f, c.do(ctx, "GET", "/api/relay/v1/folders/"+url.PathEscape(id)+q, nil, &f)
}

// EnsureFolders finds or makes path ("a/b") below parent as the person.
func (c *Client) EnsureFolders(ctx context.Context, parent, path, person, assurance string, findOnly bool) (*Folder, error) {
	body := map[string]any{"parent_id": parent, "path": path, "find_only": findOnly}
	if person != "" {
		body["on_behalf_of"] = person
	}
	if assurance != "" {
		body["assurance"] = assurance
	}
	var f Folder
	return &f, c.do(ctx, "POST", "/api/relay/v1/folders", body, &f)
}

// UploadState is what the drive knows of an upload this node delivered.
type UploadState struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"`
	NodeID  string  `json:"node_id"`
	Size    int64   `json:"size"`
	SHA256  *string `json:"sha256"`
	Current bool    `json:"current"`
	Trashed bool    `json:"trashed"`
}

func (c *Client) RelayUpload(ctx context.Context, versionID string) (*UploadState, error) {
	var s UploadState
	return &s, c.do(ctx, "GET", "/api/relay/v1/uploads/"+url.PathEscape(versionID), nil, &s)
}

// ---- Linking (login) ----

type LinkStart struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

type LinkResult struct {
	Token    string  `json:"token"`
	NodeID   string  `json:"node_id"`
	NodeName string  `json:"node_name"`
	DeviceID string  `json:"device_id"`
	Person   UserRef `json:"person"`
}

func (c *Client) EnrollStart(ctx context.Context, machine, version string) (*LinkStart, error) {
	var s LinkStart
	return &s, c.do(ctx, "POST", "/api/relay/v1/enroll", map[string]string{"client": "olsyn-drive", "machine": machine, "app_version": version}, &s)
}

// EnrollToken polls once: *Error 428 while the person hasn't approved yet.
func (c *Client) EnrollToken(ctx context.Context, deviceCode string) (*LinkResult, error) {
	var r LinkResult
	return &r, c.do(ctx, "POST", "/api/relay/v1/enroll/token", map[string]string{"device_code": deviceCode}, &r)
}
