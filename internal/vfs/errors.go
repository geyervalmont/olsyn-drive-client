package vfs

import (
	"context"
	"errors"
	"fmt"

	"github.com/geyervalmont/olsyn-drive-client/internal/cache"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// Kind classifies file system errors. Mount adapters map kinds to their
// OS's codes (NTSTATUS, errno); vfs never returns OS codes.
type Kind int

// Error kinds.
const (
	KindOther            Kind = iota // unexpected; an I/O error to the OS
	KindNotFound                     // no such file or folder
	KindExists                       // already exists
	KindNotDir                       // a folder was expected
	KindIsDir                        // a file was expected
	KindNotEmpty                     // folder not empty
	KindAccessDenied                 // read-only, refused by the drive's rules, no permission
	KindSharingViolation             // another open's share mode forbids this
	KindOffline                      // needs the drive, which cannot be reached now
	KindTimeout                      // the drive did not answer in time (e.g. first listing still loading)
	KindNoSpace                      // unsent data fills the cache
	KindInvalidName                  // a name the drive cannot store
	KindSignedOut                    // this PC is no longer linked
	KindInvalid                      // a bad argument (negative size, write to a directory handle, …)
)

var kindNames = map[Kind]string{
	KindOther: "error", KindNotFound: "not found", KindExists: "exists", KindNotDir: "not a folder",
	KindIsDir: "is a folder", KindNotEmpty: "folder not empty", KindAccessDenied: "access denied",
	KindSharingViolation: "in use", KindOffline: "offline", KindTimeout: "timed out",
	KindNoSpace: "no space", KindInvalidName: "invalid name", KindSignedOut: "signed out", KindInvalid: "invalid",
}

func (k Kind) String() string { return kindNames[k] }

// Error is a file system error.
type Error struct {
	Kind Kind
	Op   string
	Path string
	Err  error
}

func (e *Error) Error() string {
	s := "vfs: " + e.Op
	if e.Path != "" {
		s += " " + e.Path
	}
	s += ": " + e.Kind.String()
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the kind of err (KindOther for foreign errors).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindOther
}

func newErr(kind Kind, op, path string, err error) error {
	return &Error{Kind: kind, Op: op, Path: path, Err: err}
}

// classify turns a lower-level error into a vfs error.
func classify(op, path string, err error) error {
	if err == nil {
		return nil
	}
	var ve *Error
	if errors.As(err, &ve) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return newErr(KindTimeout, op, path, err)
	case errors.Is(err, cache.ErrNoSpace):
		return newErr(KindNoSpace, op, path, err)
	case driveapi.IsAuthLost(err):
		return newErr(KindSignedOut, op, path, err)
	case driveapi.IsNetwork(err), driveapi.IsRetryable(err):
		return newErr(KindOffline, op, path, err)
	case driveapi.IsNotFound(err):
		return newErr(KindNotFound, op, path, err)
	case driveapi.IsPermanent(err):
		return newErr(KindAccessDenied, op, path, err)
	}
	return newErr(KindOther, op, path, fmt.Errorf("%w", err))
}
