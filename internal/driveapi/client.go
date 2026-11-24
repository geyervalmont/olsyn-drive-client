// Package driveapi is a typed client for the Olsyn drive's HTTP API, as the
// desktop client uses it: device linking, the mount surface, node listings,
// the changes feed and its SSE wake-ups, ranged downloads, uploads and the
// signed release manifest.
//
// The client retries only one thing by itself: a 429 rate_limited answer is
// waited out (Retry-After, shared by the requests of the same route class,
// bounded; see pace.go), and it keeps each class under the drive's limits. Everything else is the caller's decision, with the
// classification helpers (IsRetryable, IsPermanent, IsConflict, IsAuthLost)
// and Backoff.
// The bearer token comes from a callback on every request and is never logged.
package driveapi

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public drive. It is only a default: every client is
// configured with the drive it talks to.
const DefaultBaseURL = "https://drive.olsyn.com"

// TokenFunc returns the bearer token for the next request, or "" when the
// device is not linked. It is called for every authenticated request.
type TokenFunc func(ctx context.Context) (string, error)

// Options configure a Client.
type Options struct {
	// BaseURL is the drive's site, e.g. DefaultBaseURL.
	BaseURL string
	// Token supplies the bearer token.
	Token TokenFunc
	// HTTPClient is used for every request (API and presigned storage URLs).
	// Its Timeout must be 0 (or long): the client uses contexts, and
	// downloads, uploads and the SSE stream run for a long time.
	HTTPClient *http.Client
	// UserAgent, e.g. "olsyn-drive/1.0 (windows)".
	UserAgent string
	// Logger for request summaries (method, path, status, duration). Nil
	// means slog.Default(). Tokens, query strings of presigned URLs and
	// bodies are never logged.
	Logger *slog.Logger
	// CheckPerMinute paces pre-check calls (POST /api/v1/uploads/check) to
	// stay under the drive's limit of CheckRateLimit a minute per person:
	// 0 means CheckPacePerMinute, a negative value no pacing (tests).
	CheckPerMinute int
	// Limits are the drive's limits by route class, in calls a minute; no
	// class uses more than 80% of its limit. Nil means DefaultLimits(); an
	// empty map none known (tests). A class without one learns it from its
	// first 429.
	Limits Limits
}

// Client talks to one drive.
type Client struct {
	base  *url.URL
	token TokenFunc
	http  *http.Client
	ua    string
	log   *slog.Logger
	pace  *pacer  // the drive's 429 waits and the limits' guards, by route class
	check *bucket // pre-check pacing; nil: none
	// rateRetries is how many 429s one call waits out (tests lower it)
	rateRetries int
}

// New returns a client for the drive at opts.BaseURL.
func New(opts Options) (*Client, error) {
	raw := opts.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, fmt.Errorf("drive base URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("drive base URL %q: scheme must be https", raw)
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	tok := opts.Token
	if tok == nil {
		tok = func(context.Context) (string, error) { return "", nil }
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "olsyn-drive"
	}
	per := opts.CheckPerMinute
	if per == 0 {
		per = CheckPacePerMinute
	}
	limits := opts.Limits
	if limits == nil {
		limits = DefaultLimits()
	}
	return &Client{base: u, token: tok, http: hc, ua: ua, log: log, pace: newPacer(limits), check: newBucket(per, checkBurst),
		rateRetries: rateRetries}, nil
}

// BaseURL returns the drive's site URL.
func (c *Client) BaseURL() string { return c.base.String() }

// request describes one API call.
type request struct {
	op      string
	method  string
	path    string // under the base URL, already escaped
	query   url.Values
	body    any // JSON-encoded when non-nil
	header  http.Header
	noAuth  bool
	accept2 []int // extra success statuses besides 2xx
	// noRateWait returns a 429 at once instead of waiting it out (a call
	// that is repeated on its own schedule, like the heartbeat)
	noRateWait bool
}

func (c *Client) url(path string, q url.Values) string {
	s := c.base.String() + path
	if len(q) > 0 {
		s += "?" + q.Encode()
	}
	return s
}

// PaceStats reports what the pacing of one route class has done.
func (c *Client) PaceStats(class Class) ClassStats { return c.pace.stats(class) }

// send performs the request and returns the response for statuses below 300
// (and the extra accepted ones). Other statuses become *Error. A 429 is
// waited out and sent again (rateRetries times) before it is returned; the
// wait holds only the request's route class.
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	var body []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, fmt.Errorf("drive: %s: encode body: %w", r.op, err)
		}
		body = b
	}
	class := classify(r.method, r.path)
	for n := 0; ; n++ {
		if r.noRateWait {
			c.pace.note(class) // it goes now: the caller has its own schedule
		} else if err := c.pace.wait(ctx, class); err != nil {
			return nil, &Error{Op: r.op, Err: err}
		}
		resp, err := c.sendOnce(ctx, r, body)
		var e *Error
		if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
			return resp, err
		}
		d := rateWait(n, e.RetryAfter)
		c.pace.hold(class, d, e.RetryAfter)
		if r.noRateWait || n >= c.rateRetries {
			return resp, err
		}
		c.log.InfoContext(ctx, "drive asked to wait", "op", r.op, "class", class.String(), "wait_ms", d.Milliseconds(), "attempt", n+1)
	}
}

func (c *Client) sendOnce(ctx context.Context, r request, b []byte) (*http.Response, error) {
	var body io.Reader
	if b != nil {
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.url(r.path, r.query), body)
	if err != nil {
		return nil, fmt.Errorf("drive: %s: %w", r.op, err)
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", c.ua)
	if !r.noAuth {
		tok, err := c.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("drive: %s: get token: %w", r.op, err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.DebugContext(ctx, "drive request failed", "op", r.op, "method", r.method, "path", r.path, "err", redactURLError(err))
		return nil, &Error{Op: r.op, Err: redactURLError(err)}
	}
	c.log.DebugContext(ctx, "drive request", "op", r.op, "method", r.method, "path", r.path,
		"status", resp.StatusCode, "ms", time.Since(start).Milliseconds())
	if resp.StatusCode < 300 {
		return resp, nil
	}
	for _, s := range r.accept2 {
		if resp.StatusCode == s {
			return resp, nil
		}
	}
	defer drainClose(resp.Body)
	return nil, readError(r.op, resp, false)
}

// do sends the request and decodes a JSON answer into out (when non-nil).
func (c *Client) do(ctx context.Context, r request, out any) error {
	resp, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Op: r.op, Status: 0, Err: fmt.Errorf("decode answer: %w", err)}
	}
	return nil
}

// readError turns a non-2xx answer into *Error. Bodies are read up to 64 KiB.
func readError(op string, resp *http.Response, storage bool) *Error {
	e := &Error{Op: op, Status: resp.StatusCode, Storage: storage, RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if storage {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		if xml.Unmarshal(b, &x) == nil {
			e.Code, e.Message = x.Code, x.Message
		}
		return e
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) == nil {
		if raw, ok := m["error"]; ok {
			_ = json.Unmarshal(raw, &e.Code)
			delete(m, "error")
		}
		if raw, ok := m["message"]; ok {
			_ = json.Unmarshal(raw, &e.Message)
			delete(m, "message")
		}
		if len(m) > 0 {
			e.Extra = m
		}
	}
	if e.Code == "" {
		e.Code = statusCode(resp.StatusCode)
	}
	return e
}

// statusCode is a code for answers without a JSON body (a proxy's 502, …).
func statusCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodeForbidden
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusTooManyRequests:
		return CodeRateLimited
	}
	if status >= 500 {
		return CodeInternal
	}
	return ""
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// redactURLError strips the URL (which may be presigned) from a transport
// error, keeping the cause.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 256<<10))
	_ = rc.Close()
}

func pathID(prefix, id, suffix string) string {
	return prefix + url.PathEscape(id) + suffix
}
