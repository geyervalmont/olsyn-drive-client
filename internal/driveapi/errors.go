package driveapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Error is every failure the drive (or the storage behind a presigned URL)
// reports, plus network failures that never got an answer. Branch on it with
// the classification helpers below rather than on Status or Code directly.
type Error struct {
	// Op names what the client was doing, e.g. "start upload".
	Op string
	// Status is the HTTP status, or 0 when no response arrived.
	Status int
	// Code is the server's "error" field (or the storage XML <Code>).
	Code string
	// Message is the server's text for people. It never contains a token.
	Message string
	// Extra holds any other fields of the error body (managed_by, sealed_folder, …).
	Extra map[string]json.RawMessage
	// RetryAfter is the server's Retry-After, when sent.
	RetryAfter time.Duration
	// Storage is true when the answer came from object storage (a presigned URL),
	// not from the drive API.
	Storage bool
	// Err is the underlying transport error when Status is 0.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("drive: ")
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	switch {
	case e.Status == 0 && e.Err != nil:
		b.WriteString(e.Err.Error())
	default:
		fmt.Fprintf(&b, "%d", e.Status)
		if e.Code != "" {
			b.WriteString(" ")
			b.WriteString(e.Code)
		}
		if e.Message != "" {
			b.WriteString(": ")
			b.WriteString(e.Message)
		}
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// ExtraString returns an extra field as a string: a JSON string as is, an
// object's "name" field, or "" when absent.
func (e *Error) ExtraString(key string) string {
	raw, ok := e.Extra[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Name
	}
	return ""
}

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Codes the client branches on.
const (
	CodeUnauthenticated   = "unauthenticated"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeNameConflict      = "name_conflict"
	CodeVersionConflict   = "version_conflict"
	CodeUploadIncomplete  = "upload_incomplete"
	CodeUploadExpired     = "upload_expired"
	CodeArchived          = "archived"
	CodeInvalid           = "invalid"
	CodeInvalidName       = "invalid_name"
	CodeInvalidModifiedAt = "invalid_modified_at"
	CodeChecksumMismatch  = "checksum_mismatch"
	CodeQuotaExceeded     = "quota_exceeded"
	CodeLegalHold         = "legal_hold"
	CodeRetentionPeriod   = "retention_period"
	CodeWritesPaused      = "writes_paused"
	CodeManaged           = "managed"
	CodeSealed            = "sealed"
	CodeGeneratedWrite    = "generated_write_recorded"
	CodeAuthPending       = "authorization_pending"
	CodeRateLimited       = "rate_limited"
	CodeAccessUnavailable = "access_unavailable"
	CodeBuilding          = "building"
	CodeInternal          = "internal"
	// Storage (S3-compatible) codes seen on presigned URLs.
	CodeNoSuchUpload = "NoSuchUpload"
)

// IsRetryable reports a failure that is expected to go away by itself:
// no answer at all, a 5xx, 429, 503 access_unavailable/building, or
// 423 writes_paused (an admin paused changes: wait, do not give up).
// A cancelled context is never retryable.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	e, ok := AsError(err)
	if !ok {
		return false
	}
	switch {
	case e.Status == 0:
		return !errors.Is(e.Err, context.Canceled)
	case e.Status == http.StatusTooManyRequests:
		return true
	case e.Status == http.StatusLocked && e.Code == CodeWritesPaused:
		return true
	case e.Status == http.StatusNotImplemented:
		return false
	case e.Status >= 500 && e.Status != http.StatusInsufficientStorage:
		return true
	}
	return false
}

// IsWait reports the retryable failures that mean "the server asked us to
// wait" (503 with Retry-After, 429, writes_paused) rather than an outage.
func IsWait(err error) bool {
	e, ok := AsError(err)
	if !ok {
		return false
	}
	return e.Code == CodeWritesPaused || e.Status == http.StatusTooManyRequests ||
		e.Code == CodeAccessUnavailable || e.Code == CodeBuilding || e.RetryAfter > 0
}

// permanentCodes are refusals that will not change by retrying.
var permanentCodes = map[string]string{
	CodeManaged:           "This file is managed by an integration and can't be changed from this PC.",
	CodeSealed:            "This folder is sealed: nothing inside it can be added, changed, moved or deleted.",
	CodeForbidden:         "You don't have permission to change this on the drive.",
	CodeLegalHold:         "This item is under a legal hold and can't be changed or deleted.",
	CodeRetentionPeriod:   "This item is inside a retention period and can't be deleted yet.",
	CodeQuotaExceeded:     "The folder's storage quota is full.",
	CodeInvalidName:       "The drive doesn't accept this name.",
	CodeArchived:          "This file is in deep archive; restore it from the web app first.",
	CodeGeneratedWrite:    "This is a generated folder: the drive recorded the change but keeps the generated file.",
	CodeInvalidModifiedAt: "The file's modified time is outside what the drive accepts.",
}

// ChecksumMismatchReason is the plain words for a file the drive received
// with other bytes than this PC sent.
const ChecksumMismatchReason = "The drive received different bytes from those this PC sent, so the upload was not kept. Nothing on the drive was replaced. Re-upload to try again."

// IsPermanent reports a refusal that will never succeed by retrying:
// managed, sealed, forbidden, legal hold, retention period, quota,
// invalid name, archived (and other 4xx that are not conflicts, auth or
// "not found"). Hold the change and tell the person; never retry it.
func IsPermanent(err error) bool {
	e, ok := AsError(err)
	if !ok || e.Storage {
		return false
	}
	if _, ok := permanentCodes[e.Code]; ok {
		return true
	}
	if e.Code == CodeWritesPaused || IsConflict(err) || IsAuthLost(err) {
		return false
	}
	switch e.Status {
	case http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusLocked:
		return e.Code != CodeChecksumMismatch
	}
	return false
}

// IsConflict reports 409 version_conflict or name_conflict.
func IsConflict(err error) bool {
	e, ok := AsError(err)
	return ok && !e.Storage && (e.Code == CodeVersionConflict || e.Code == CodeNameConflict)
}

// CodeVersionChanged: the bytes on the drive are no longer the ones asked for.
const CodeVersionChanged = "version_changed"

// IsVersionChanged reports a read that found other bytes than the ones it
// was pinned to.
func IsVersionChanged(err error) bool {
	e, ok := AsError(err)
	return ok && e.Code == CodeVersionChanged
}

// IsVersionConflict reports 409 version_conflict.
func IsVersionConflict(err error) bool {
	e, ok := AsError(err)
	return ok && e.Code == CodeVersionConflict
}

// IsNameConflict reports 409 name_conflict.
func IsNameConflict(err error) bool {
	e, ok := AsError(err)
	return ok && e.Code == CodeNameConflict
}

// IsInvalidModifiedAt reports the drive refused a client modified time
// (out of its range, or finer than a millisecond).
func IsInvalidModifiedAt(err error) bool {
	e, ok := AsError(err)
	return ok && e.Code == CodeInvalidModifiedAt
}

// IsAuthLost reports 401: the token is unknown, revoked or expired.
// The person must link this PC again.
func IsAuthLost(err error) bool {
	e, ok := AsError(err)
	return ok && !e.Storage && e.Status == http.StatusUnauthorized
}

// IsNotFound reports 404 from the drive (gone, or not readable by us).
func IsNotFound(err error) bool {
	e, ok := AsError(err)
	return ok && !e.Storage && e.Status == http.StatusNotFound
}

// IsUploadGone reports an upload session the server no longer has:
// 410 upload_expired, 404 on an upload route, or storage NoSuchUpload.
// Start the upload again.
func IsUploadGone(err error) bool {
	e, ok := AsError(err)
	if !ok {
		return false
	}
	if e.Storage {
		return e.Code == CodeNoSuchUpload || e.Status == http.StatusNotFound
	}
	return e.Code == CodeUploadExpired
}

// IsStorageURLExpired reports a presigned URL that storage refused (403):
// ask the drive for fresh part URLs.
func IsStorageURLExpired(err error) bool {
	e, ok := AsError(err)
	return ok && e.Storage && e.Status == http.StatusForbidden
}

// IsNetwork reports a failure with no HTTP answer.
func IsNetwork(err error) bool {
	e, ok := AsError(err)
	return ok && e.Status == 0
}

// RetryAfter returns the server's Retry-After, or 0.
func RetryAfter(err error) time.Duration {
	if e, ok := AsError(err); ok {
		return e.RetryAfter
	}
	return 0
}

// Reason is a plain-English sentence for the UI explaining why the drive
// refused a change. It is empty for errors that are not refusals.
func Reason(err error) string {
	e, ok := AsError(err)
	if !ok {
		return ""
	}
	if s, ok := permanentCodes[e.Code]; ok {
		switch e.Code {
		case CodeManaged:
			if by := e.ExtraString("managed_by"); by != "" {
				return "This file is managed by " + by + " and can't be changed from this PC."
			}
		case CodeSealed:
			if name := e.ExtraString("sealed_folder"); name != "" {
				return "The folder \"" + name + "\" is sealed: nothing inside it can be added, changed, moved or deleted."
			}
		}
		return s
	}
	switch {
	case e.Code == CodeChecksumMismatch:
		return ChecksumMismatchReason
	case IsConflict(err):
		if e.Code == CodeVersionConflict {
			return "Someone else saved a newer version of this file."
		}
		return "Something with this name already exists in the folder."
	case IsAuthLost(err):
		return "This PC was signed out of the drive."
	case IsPermanent(err):
		if e.Message != "" {
			return "The drive refused the change: " + e.Message
		}
		return "The drive refused the change."
	}
	return ""
}
