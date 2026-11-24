package transfer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func setupWith(t *testing.T, opts fakedrive.Options) (*fakedrive.Server, *driveapi.Client) {
	t.Helper()
	fd := fakedrive.New(opts)
	t.Cleanup(fd.Close)
	api, err := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(ctx context.Context) (string, error) { return fd.Token(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return fd, api
}

// Single-PUT and multipart uploads both send the SHA-256 at completion:
// small ones are verified at once, large ones claimed and then verified by
// the drive's pass.
func TestEveryUploadSendsSHA256(t *testing.T) {
	fd, api := setupWith(t, fakedrive.Options{PartSize: partSize, SingleMax: 100 << 10, VerifyMax: 200 << 10})
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	small := payload(50<<10, 1)
	big := payload(11*partSize+3, 2)
	for _, f := range []struct {
		name string
		data []byte
		want string
	}{{"small.bin", small, driveapi.ChecksumVerified}, {"big.bin", big, driveapi.ChecksumClaimed}} {
		if _, err := eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: f.name, Size: int64(len(f.data)),
			Src: bytes.NewReader(f.data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-" + f.name}); err != nil {
			t.Fatal(err)
		}
		fi, _ := fd.Lookup("/" + f.name)
		if fi.ChecksumState != f.want {
			t.Fatalf("%s: checksum_state %q, want %q", f.name, fi.ChecksumState, f.want)
		}
	}
	if fd.Count("upload.sha256_sent") != 2 {
		t.Fatalf("sha256 sent %d times", fd.Count("upload.sha256_sent"))
	}
	fd.RunChecksumPass()
	if fi, _ := fd.Lookup("/big.bin"); fi.ChecksumState != driveapi.ChecksumVerified || fi.SHA256 != sum(big) {
		t.Fatalf("big.bin after the pass: %q", fi.ChecksumState)
	}
}

// One checksum_mismatch: the session is aborted and the file sent again.
// Two: ErrChecksumMismatch, with the drive's code, and nothing published.
func TestChecksumMismatchUploadsAgainOnce(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	data := payload(5*partSize, 3)
	fd.CorruptNextUploads(1)
	if _, err := eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: "once.bin", Size: int64(len(data)),
		Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-once"}); err != nil {
		t.Fatal(err)
	}
	if fi, ok := fd.Lookup("/once.bin"); !ok || fi.SHA256 != sum(data) || fd.PendingUploads() != 0 {
		t.Fatal("once.bin not stored after one mismatch, or a session left open")
	}
	fd.CorruptNextUploads(2)
	_, err := eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: "twice.bin", Size: int64(len(data)),
		Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-twice"})
	if !errors.Is(err, transfer.ErrChecksumMismatch) {
		t.Fatalf("err %v", err)
	}
	if e, ok := driveapi.AsError(err); !ok || e.Code != driveapi.CodeChecksumMismatch {
		t.Fatalf("drive code lost: %v", err)
	}
	if _, ok := fd.Lookup("/twice.bin"); ok || fd.PendingUploads() != 0 {
		t.Fatal("twice.bin published, or a session left open")
	}
}
