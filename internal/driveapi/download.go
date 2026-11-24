package driveapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Download is an open ranged read. Body yields exactly Length bytes from
// Offset, or fails with a retryable *Error (io.ErrUnexpectedEOF inside) when
// the connection drops early. Close it.
type Download struct {
	Body   io.ReadCloser
	Offset int64
	Length int64
	// Total is the file's size.
	Total int64
	// ETag is the strong validator, "v-<version id>" for drive files.
	ETag string
}

// VersionETag is the drive's ETag for a version's bytes.
func VersionETag(versionID string) string { return `"v-` + versionID + `"` }

// Download reads bytes [off, off+length) of a file version through the mount
// content route (access is checked on every read; never a presigned URL).
// length <= 0 reads to the end. versionID pins the version: when set,
// If-Range makes a changed validator fail loudly instead of mixing versions.
// A range starting at or past the end answers an *Error with Status 416.
// versionID may be a generated identity (IsGeneratedVersion): reading a
// generated file that is not built yet makes the drive build it.
func (c *Client) Download(ctx context.Context, nodeID, versionID string, off, length int64) (*Download, error) {
	q := url.Values{}
	// A generated file has no versions: no version parameter, and the
	// validator is the ETag an earlier read answered with, when known.
	ifRange := ""
	switch {
	case IsGeneratedETagVersion(versionID):
		ifRange = strings.TrimPrefix(versionID, generatedByETag)
	case IsGeneratedVersion(versionID):
	case versionID != "":
		q.Set("version", versionID)
		ifRange = VersionETag(versionID)
	}
	h := http.Header{"Accept": {"*/*"}}
	if off > 0 || length > 0 {
		r := "bytes=" + strconv.FormatInt(off, 10) + "-"
		if length > 0 {
			r += strconv.FormatInt(off+length-1, 10)
		}
		h.Set("Range", r)
		if ifRange != "" {
			h.Set("If-Range", ifRange)
		}
	}
	resp, err := c.send(ctx, request{op: "download", method: http.MethodGet, path: pathID("/api/mount/v1/nodes/", nodeID, "/content"), query: q, header: h})
	if err != nil {
		return nil, err
	}
	d := &Download{ETag: resp.Header.Get("ETag")}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			drainClose(resp.Body)
			return nil, &Error{Op: "download", Status: resp.StatusCode, Err: err}
		}
		if start != off {
			drainClose(resp.Body)
			return nil, &Error{Op: "download", Status: resp.StatusCode, Err: fmt.Errorf("asked for offset %d, got %d", off, start)}
		}
		d.Offset, d.Length, d.Total = start, end-start+1, total
	case http.StatusOK:
		// The whole file: the server ignored the range (or the validator
		// changed). Skip to off.
		d.Total = resp.ContentLength
		if d.Total < 0 {
			drainClose(resp.Body)
			return nil, &Error{Op: "download", Status: resp.StatusCode, Err: fmt.Errorf("no length on a full answer")}
		}
		if ifRange != "" && d.ETag != "" && d.ETag != ifRange {
			drainClose(resp.Body)
			return nil, &Error{Op: "download", Status: http.StatusPreconditionFailed, Code: "version_changed", Err: fmt.Errorf("server sent another version")}
		}
		if off > 0 {
			if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
				drainClose(resp.Body)
				return nil, &Error{Op: "download", Err: fmt.Errorf("skip to offset: %w", err)}
			}
		}
		d.Offset = off
		d.Length = d.Total - off
		if length > 0 && length < d.Length {
			d.Length = length
		}
	default:
		drainClose(resp.Body)
		return nil, &Error{Op: "download", Status: resp.StatusCode, Err: fmt.Errorf("unexpected status")}
	}
	d.Body = &exactBody{rc: resp.Body, left: d.Length}
	return d, nil
}

// exactBody yields exactly left bytes and turns early ends into a
// retryable network *Error.
type exactBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *exactBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if err == io.EOF {
		if b.left > 0 {
			return n, &Error{Op: "download", Err: io.ErrUnexpectedEOF}
		}
		return n, io.EOF
	}
	if err != nil {
		return n, &Error{Op: "download", Err: redactURLError(err)}
	}
	if b.left == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (b *exactBody) Close() error { return b.rc.Close() }

func parseContentRange(v string) (start, end, total int64, err error) {
	// bytes a-b/total
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	rng, tot, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	a, b, ok := strings.Cut(rng, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
		return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	total = -1
	if tot != "*" {
		if total, err = strconv.ParseInt(tot, 10, 64); err != nil {
			return 0, 0, 0, fmt.Errorf("bad Content-Range %q", v)
		}
	}
	return start, end, total, nil
}
