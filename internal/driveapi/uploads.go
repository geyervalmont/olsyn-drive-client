package driveapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Upload methods.
const (
	MethodSingle    = "single"
	MethodMultipart = "multipart"
)

// StartUpload is the body of POST /api/v1/uploads.
type StartUpload struct {
	ParentID      string `json:"parent_id"`
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Mime          string `json:"mime,omitempty"`
	Conflict      string `json:"conflict,omitempty"`
	BaseVersionID string `json:"base_version_id,omitempty"`
	// Fingerprint (≤ 200 chars): starting again with the same parent, size
	// and fingerprint returns the unfinished upload (Resumed true).
	Fingerprint string `json:"fingerprint,omitempty"`
	ModifiedAt  *Time  `json:"modified_at,omitempty"`
}

// PartURL is a presigned URL for one part.
type PartURL struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// UploadedPart is a part storage already holds.
type UploadedPart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// UploadTicket is the answer to starting (or resuming) an upload.
type UploadTicket struct {
	UploadID  string            `json:"upload_id"`
	NodeID    string            `json:"node_id"`
	VersionID string            `json:"version_id"`
	Name      string            `json:"name"`
	Method    string            `json:"method"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	PartSize  int64             `json:"part_size,omitempty"`
	PartCount int               `json:"part_count,omitempty"`
	Parts     []PartURL         `json:"parts,omitempty"`
	Uploaded  []UploadedPart    `json:"uploaded,omitempty"`
	Resumed   bool              `json:"resumed"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// StartUpload starts an upload, or resumes the caller's unfinished one with
// the same parent, size and fingerprint.
func (c *Client) StartUpload(ctx context.Context, req StartUpload) (*UploadTicket, error) {
	var out UploadTicket
	if err := c.do(ctx, request{op: "start upload", method: http.MethodPost, path: "/api/v1/uploads", body: req}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadPartsMax is the most part URLs one request may ask for.
const UploadPartsMax = 100

// PartURLs asks for fresh presigned URLs for up to 100 parts.
func (c *Client) PartURLs(ctx context.Context, uploadID string, numbers []int) ([]PartURL, error) {
	if len(numbers) > UploadPartsMax {
		return nil, fmt.Errorf("drive: part urls: %d parts asked, at most %d", len(numbers), UploadPartsMax)
	}
	var out struct {
		Parts []PartURL `json:"parts"`
	}
	err := c.do(ctx, request{op: "part urls", method: http.MethodPost, path: pathID("/api/v1/uploads/", uploadID, "/parts"),
		body: map[string]any{"numbers": numbers}}, &out)
	if err != nil {
		return nil, err
	}
	return out.Parts, nil
}

// CompleteUpload is the body of POST /uploads/{id}/complete. Parts is
// optional (storage's list is the truth).
type CompleteUpload struct {
	Parts      []UploadedPart `json:"-"`
	ModifiedAt *Time          `json:"modified_at,omitempty"`
	// SHA256 (64 hex) makes the server verify small files (≤ 64 MiB) and
	// record a claim on larger ones.
	SHA256 string `json:"sha256,omitempty"`
}

// Complete finishes an upload and returns the file. Completing twice is safe.
func (c *Client) Complete(ctx context.Context, uploadID string, req CompleteUpload) (*Node, error) {
	body := map[string]any{}
	if len(req.Parts) > 0 {
		ps := make([]map[string]any, len(req.Parts))
		for i, p := range req.Parts {
			ps[i] = map[string]any{"number": p.Number, "etag": p.ETag}
		}
		body["parts"] = ps
	}
	if req.ModifiedAt != nil {
		body["modified_at"] = req.ModifiedAt
	}
	if req.SHA256 != "" {
		body["sha256"] = req.SHA256
	}
	var out Node
	err := c.do(ctx, request{op: "complete upload", method: http.MethodPost, path: pathID("/api/v1/uploads/", uploadID, "/complete"), body: body}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AbortUpload cancels an unfinished upload.
func (c *Client) AbortUpload(ctx context.Context, uploadID string) error {
	return c.do(ctx, request{op: "abort upload", method: http.MethodDelete, path: pathID("/api/v1/uploads/", uploadID, "")}, nil)
}

// UploadStatus is an unfinished upload as the server sees it.
type UploadStatus struct {
	UploadID      string         `json:"upload_id"`
	NodeID        string         `json:"node_id"`
	ParentID      string         `json:"parent_id"`
	Name          string         `json:"name"`
	Size          int64          `json:"size"`
	Mime          string         `json:"mime"`
	Method        string         `json:"method"`
	PartSize      int64          `json:"part_size"`
	PartCount     int            `json:"part_count"`
	Uploaded      []UploadedPart `json:"uploaded"`
	UploadedBytes int64          `json:"uploaded_bytes"`
	Fingerprint   string         `json:"fingerprint"`
	CreatedAt     time.Time      `json:"created_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
}

// GetUpload reads an unfinished upload's state (which parts storage holds).
func (c *Client) GetUpload(ctx context.Context, uploadID string) (*UploadStatus, error) {
	var out UploadStatus
	if err := c.do(ctx, request{op: "upload status", method: http.MethodGet, path: pathID("/api/v1/uploads/", uploadID, "")}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListUploads lists the caller's unfinished uploads, newest first.
func (c *Client) ListUploads(ctx context.Context) ([]UploadStatus, error) {
	var out struct {
		Items []UploadStatus `json:"items"`
	}
	if err := c.do(ctx, request{op: "list uploads", method: http.MethodGet, path: "/api/v1/uploads"}, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// CheckFile is one file of a pre-check.
type CheckFile struct {
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Fingerprint string `json:"fingerprint,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	ModifiedAt  *Time  `json:"modified_at,omitempty"`
}

// Pre-check states.
const (
	CheckPresent   = "present"
	CheckDifferent = "different"
	CheckAbsent    = "absent"
)

// CheckResult is the pre-check's answer for one file.
type CheckResult struct {
	State    string `json:"state"`
	ID       string `json:"id,omitempty"`
	SameSize bool   `json:"same_size,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Probable bool   `json:"probable,omitempty"`
}

// The drive's limits on POST /api/v1/uploads/check (docs/api.md): a call
// carries at most CheckMaxFiles files in at most CheckMaxFolders distinct
// folders (422 beyond either), and a person makes at most CheckRateLimit
// calls a minute (429 with Retry-After beyond). The bootstrap does not
// publish them, so they are constants here.
const (
	CheckMaxFiles   = 1000
	CheckMaxFolders = 200
	CheckRateLimit  = 120
	// CheckPacePerMinute is the client's own pace, under the limit.
	CheckPacePerMinute = 100
	// checkBurst calls may go at once before the pace applies: at most
	// checkBurst + CheckPacePerMinute (110) in any minute.
	checkBurst = 10
)

// SplitCheck cuts files into consecutive groups the drive accepts in one
// pre-check: at most CheckMaxFiles files in at most CheckMaxFolders
// distinct parent folders each. Order is kept.
func SplitCheck(files []CheckFile) [][]CheckFile {
	var out [][]CheckFile
	start := 0
	parents := map[string]bool{}
	for i, f := range files {
		if i-start == CheckMaxFiles || (!parents[f.ParentID] && len(parents) == CheckMaxFolders) {
			out = append(out, files[start:i])
			start = i
			clear(parents)
		}
		parents[f.ParentID] = true
	}
	if start < len(files) {
		out = append(out, files[start:])
	}
	return out
}

// CheckUploads asks which files are already on the drive, in order. Any
// number of files may be given: they go in as few calls as the drive's
// limits allow (SplitCheck), each paced to stay under its rate limit.
// mtimeHint opts in to "probable" answers from equal modified times.
func (c *Client) CheckUploads(ctx context.Context, files []CheckFile, mtimeHint bool) ([]CheckResult, error) {
	out := make([]CheckResult, 0, len(files))
	for _, group := range SplitCheck(files) {
		res, err := c.checkOnce(ctx, group, mtimeHint)
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
	}
	return out, nil
}

func (c *Client) checkOnce(ctx context.Context, files []CheckFile, mtimeHint bool) ([]CheckResult, error) {
	if err := c.check.take(ctx); err != nil {
		return nil, &Error{Op: "pre-check", Err: err}
	}
	body := map[string]any{"files": files}
	if mtimeHint {
		body["mtime_hint"] = true
	}
	var out struct {
		Items []CheckResult `json:"items"`
	}
	if err := c.do(ctx, request{op: "pre-check", method: http.MethodPost, path: "/api/v1/uploads/check", body: body}, &out); err != nil {
		return nil, err
	}
	if len(out.Items) != len(files) {
		return nil, fmt.Errorf("drive: pre-check: %d answers for %d files", len(out.Items), len(files))
	}
	return out.Items, nil
}

// PutPart sends one part (or a single upload's whole body) to its presigned
// URL with exactly the headers the ticket gave. No drive token is sent.
// It returns storage's ETag. Storage refusals come back as *Error with
// Storage set: 403 means the URL expired (ask for fresh ones); NoSuchUpload
// means the multipart upload is gone; 412 on a single upload means the
// object already exists (go on to complete).
func (c *Client) PutPart(ctx context.Context, rawURL string, headers map[string]string, body io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, body)
	if err != nil {
		return "", fmt.Errorf("drive: put part: %w", redactURLError(err))
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &Error{Op: "put part", Storage: true, Err: redactURLError(err)}
	}
	defer drainClose(resp.Body)
	if resp.StatusCode >= 300 {
		return "", readError("put part", resp, true)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// IsAlreadyStored reports a single upload's create-only PUT answered 412:
// the object exists, so go on to complete (which verifies its size).
func IsAlreadyStored(err error) bool {
	e, ok := AsError(err)
	return ok && e.Storage && e.Status == http.StatusPreconditionFailed
}
