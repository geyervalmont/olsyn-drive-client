package vfs_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A large file is published as "claimed"; when the drive's checksum pass
// later finds other bytes, this PC's copy (kept: it was never verified) is
// uploaded again on its own.
func TestDamagedLargeUploadIsSentAgain(t *testing.T) {
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10, VerifyMax: 128 << 10}).start()
	defer e.stop()
	data := payload(5*part+77, 9)
	e.fd.CorruptNextUploads(1)
	if err := e.writeFile("/big.bin", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if n := e.fd.RunChecksumPass(); n != 1 {
		t.Fatalf("pass checked %d versions", n)
	}
	e.fs.Refresh()
	waitFor(t, 10*time.Second, "the second upload", func() bool {
		fi, _ := e.fd.Lookup("/big.bin")
		return fi.Versions == 2 && e.fs.Pending() == 0
	})
	if len(e.fs.Held()) != 0 {
		t.Fatalf("held %+v", e.fs.Held())
	}
	e.fd.RunChecksumPass()
	fi, _ := e.fd.Lookup("/big.bin")
	if fi.ChecksumState != driveapi.ChecksumVerified || !bytes.Equal(fi.Data, data) {
		t.Fatalf("after the second upload: state %q", fi.ChecksumState)
	}
}

// A file the drive's checksum pass finds damaged twice is held with
// "re-upload", stays readable from this PC's copy, and re-uploading
// replaces the damaged version with a verified one.
func TestDamagedLargeUploadIsHeldForReupload(t *testing.T) {
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10, VerifyMax: 128 << 10}).start()
	defer e.stop()
	ctx := ctxT(t)
	data := payload(5*part+77, 9)
	e.fd.CorruptNextUploads(2)
	if err := e.writeFile("/big.bin", data, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	fi, _ := e.fd.Lookup("/big.bin")
	if fi.ChecksumState != driveapi.ChecksumClaimed {
		t.Fatalf("state after upload %q, want claimed (the SHA-256 was not sent)", fi.ChecksumState)
	}
	if n := e.fd.RunChecksumPass(); n != 1 {
		t.Fatalf("pass checked %d versions", n)
	}
	e.fs.Refresh()
	waitFor(t, 10*time.Second, "the automatic second upload", func() bool {
		fi, _ := e.fd.Lookup("/big.bin")
		return fi.Versions == 2 && e.fs.Pending() == 0
	})
	if n := e.fd.RunChecksumPass(); n != 1 {
		t.Fatalf("second pass checked %d versions", n)
	}
	e.fs.Refresh()
	waitFor(t, 10*time.Second, "a held re-upload", func() bool { return len(e.fs.Held()) == 1 })
	h := e.fs.Held()[0]
	if h.Code != driveapi.CodeChecksumMismatch || !h.CanReupload || !h.HasContent || h.Path != "/big.bin" {
		t.Fatalf("held %+v", h)
	}
	if got, err := e.readFile("/big.bin"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("local copy while held: %v", err)
	}
	if e.fs.Pending() == 0 {
		t.Fatal("a held re-upload must count as unsent")
	}
	if err := e.fs.Reupload(ctx, h.Seq); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if len(e.fs.Held()) != 0 {
		t.Fatalf("still held: %+v", e.fs.Held())
	}
	e.fd.RunChecksumPass()
	fi, _ = e.fd.Lookup("/big.bin")
	if fi.Versions != 3 || fi.ChecksumState != driveapi.ChecksumVerified || fi.SHA256 != hexSum(data) || !bytes.Equal(fi.Data, data) {
		t.Fatalf("after re-upload: versions %d state %q", fi.Versions, fi.ChecksumState)
	}
	// the verified result settles the claim: nothing more is held
	e.fs.Refresh()
	time.Sleep(400 * time.Millisecond)
	if len(e.fs.Held()) != 0 {
		t.Fatal("verified version held")
	}
}

// A small file the drive refuses with checksum_mismatch is sent again once;
// a second refusal holds it with re-upload, and nothing is published.
func TestChecksumMismatchAtCompleteRetriesOnceThenHolds(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	ctx := ctxT(t)
	a := payload(3*part, 1)
	e.fd.CorruptNextUploads(1)
	if err := e.writeFile("/a.bin", a, time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	fi, ok := e.fd.Lookup("/a.bin")
	if !ok || !bytes.Equal(fi.Data, a) || fi.ChecksumState != driveapi.ChecksumVerified {
		t.Fatalf("a.bin after one mismatch: ok %v state %q", ok, fi.ChecksumState)
	}
	if n := e.fd.Count("upload.checksum_mismatch"); n != 1 {
		t.Fatalf("mismatches %d", n)
	}

	b := payload(2*part+5, 2)
	e.fd.CorruptNextUploads(2)
	if err := e.writeFile("/b.bin", b, time.Time{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "a held file", func() bool { return len(e.fs.Held()) == 1 })
	h := e.fs.Held()[0]
	if h.Code != driveapi.CodeChecksumMismatch || !h.CanReupload {
		t.Fatalf("held %+v", h)
	}
	if _, ok := e.fd.Lookup("/b.bin"); ok {
		t.Fatal("a mismatched upload was published")
	}
	if e.fd.PendingUploads() != 0 {
		t.Fatalf("%d mismatched upload sessions left open", e.fd.PendingUploads())
	}
	if err := e.fs.Reupload(ctx, h.Seq); err != nil {
		t.Fatal(err)
	}
	e.waitSynced(20 * time.Second)
	if fi, ok := e.fd.Lookup("/b.bin"); !ok || !bytes.Equal(fi.Data, b) || fi.SHA256 != hexSum(b) {
		t.Fatal("b.bin after re-upload")
	}
}
