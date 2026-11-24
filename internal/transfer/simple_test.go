package transfer_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

const partSize = 64 << 10

func setup(t *testing.T) (*fakedrive.Server, *driveapi.Client) {
	t.Helper()
	fd := fakedrive.New(fakedrive.Options{PartSize: partSize, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	api, err := driveapi.New(driveapi.Options{BaseURL: fd.URL(), Token: func(context.Context) (string, error) { return fd.Token(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return fd, api
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13) ^ seed
	}
	return b
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var fastBackoff = driveapi.Backoff{Base: 20 * time.Millisecond, Max: 200 * time.Millisecond}

func TestUploadSurvivesDropsAndExpiredURLs(t *testing.T) {
	fd, api := setup(t)
	var events atomic.Int32
	eng := transfer.NewSimple(api, transfer.Options{Parallel: 3, Backoff: fastBackoff, OnEvent: func(transfer.Event) { events.Add(1) }})
	data := payload(10*partSize+123, 1)
	fd.DropNextPartPuts(2, 5000)
	expired := false
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && !expired && strings.HasSuffix(r.URL.Path, "/5") {
			expired = true
			fd.ExpirePartURLs()
		}
		return false
	})
	mt := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.UTC)
	res, err := eng.Upload(ctxT(t), transfer.UploadRequest{TransferID: "t1", ParentID: fd.RootID(), Name: "big.bin",
		Size: int64(len(data)), Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-1", ModifiedAt: mt, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	fi, ok := fd.Lookup("/big.bin")
	if !ok || !bytes.Equal(fi.Data, data) || res.Node.ID != fi.ID {
		t.Fatal("stored content differs")
	}
	if fi.ModifiedAt == nil || !fi.ModifiedAt.Equal(mt) {
		t.Fatalf("modified_at %v", fi.ModifiedAt)
	}
	if events.Load() == 0 {
		t.Fatal("no progress events")
	}
}

func TestResumeFromServerPartsAndReplayCompleted(t *testing.T) {
	fd, api := setup(t)
	data := payload(8*partSize, 2)
	// first attempt: parts from 4 on fail, the engine gives up
	var failing atomic.Bool
	failing.Store(true)
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if failing.Load() && r.Method == http.MethodPut {
			for _, p := range []string{"/4", "/5", "/6", "/7", "/8"} {
				if strings.HasSuffix(r.URL.Path, p) {
					w.WriteHeader(http.StatusInternalServerError)
					return true
				}
			}
		}
		return false
	})
	weak := transfer.NewSimple(api, transfer.Options{Parallel: 1, Attempts: 1, Backoff: fastBackoff})
	var session string
	req := transfer.UploadRequest{TransferID: "t2", ParentID: fd.RootID(), Name: "r.bin", Size: int64(len(data)),
		Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-2", OnSession: func(id string) { session = id }}
	if _, err := weak.Upload(ctxT(t), req); !driveapi.IsRetryable(err) {
		t.Fatalf("first attempt: %v", err)
	}
	if session == "" {
		t.Fatal("session id not reported")
	}
	failing.Store(false)
	before := fd.Count("part.put")
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	req.UploadID = session
	res, err := eng.Upload(ctxT(t), req)
	if err != nil {
		t.Fatal(err)
	}
	if sent := fd.Count("part.put") - before; sent != 5 {
		t.Fatalf("resent %d parts, want the 5 missing", sent)
	}
	if !res.Resumed {
		t.Fatal("not reported as resumed")
	}
	// replay after a crash that happened after completion: same file, nothing sent
	before = fd.Count("part.put")
	res2, err := eng.Upload(ctxT(t), req)
	if err != nil || res2.Node.ID != res.Node.ID || fd.Count("part.put") != before {
		t.Fatalf("replay: %v %v", res2, err)
	}
	if fi, _ := fd.Lookup("/r.bin"); fi.Versions != 1 || !bytes.Equal(fi.Data, data) {
		t.Fatalf("versions %d", fi.Versions)
	}
}

func TestDeployDuringUploadAndSessionGone(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Parallel: 2, Backoff: driveapi.Backoff{Base: 100 * time.Millisecond, Max: time.Second}})
	data := payload(6*partSize, 3)
	var once sync.Once
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			once.Do(func() { fd.Unavailable(1500 * time.Millisecond) })
		}
		return false
	})
	if _, err := eng.Upload(ctxT(t), transfer.UploadRequest{TransferID: "t3", ParentID: fd.RootID(), Name: "d.bin",
		Size: int64(len(data)), Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-3"}); err != nil {
		t.Fatalf("upload across a deploy: %v", err)
	}
	// the session disappears mid-upload (reaped): the engine starts again
	fd.SetIntercept(nil)
	var aborted atomic.Bool
	fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/3") && !aborted.Swap(true) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<Error><Code>NoSuchUpload</Code></Error>"))
			return true
		}
		return false
	})
	if _, err := eng.Upload(ctxT(t), transfer.UploadRequest{TransferID: "t4", ParentID: fd.RootID(), Name: "g.bin",
		Size: int64(len(data)), Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-4"}); err != nil {
		t.Fatalf("restart after NoSuchUpload: %v", err)
	}
	if fi, ok := fd.Lookup("/g.bin"); !ok || !bytes.Equal(fi.Data, data) {
		t.Fatal("g.bin differs")
	}
}

func TestPreCheckSkipsAndRefusalIsPermanent(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	data := payload(1000, 4)
	fd.AddFile(fd.RootID(), "same.txt", data)
	res, err := eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: "same.txt", Size: 1000, Src: bytes.NewReader(data), PreCheck: true})
	if err != nil || !res.Skipped {
		t.Fatalf("pre-check: %+v %v", res, err)
	}
	sealed := fd.AddFolder(fd.RootID(), "Sealed")
	fd.RefuseWrites(sealed, driveapi.CodeSealed)
	starts := fd.Count("upload.start")
	_, err = eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: sealed, Name: "x.txt", Size: 1000, Src: bytes.NewReader(data)})
	if !driveapi.IsPermanent(err) || fd.Count("upload.start")-starts != 1 {
		t.Fatalf("sealed: %v (starts %d)", err, fd.Count("upload.start")-starts)
	}
}

func TestDownloadResumesAfterDrops(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff, ChunkSize: 100 << 10})
	data := payload(350<<10, 5)
	id := fd.AddFile(fd.RootID(), "dl.bin", data)
	fi, _ := fd.Lookup("/dl.bin")
	fd.DropNextDownloads(3, 30000)
	buf := &transfer.BufferAt{Buf: make([]byte, 300<<10), Base: 20 << 10}
	if err := eng.Download(ctxT(t), transfer.DownloadRequest{NodeID: id, VersionID: fi.VersionID, Offset: 20 << 10, Length: 300 << 10, Dst: buf}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Buf, data[20<<10:320<<10]) {
		t.Fatal("downloaded bytes differ")
	}
}

func TestPauseAndBandwidth(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	data := payload(256<<10, 6)
	eng.SetBandwidth(512<<10, 0)
	eng.Pause()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := eng.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: "p.bin", Size: int64(len(data)),
			Src: bytes.NewReader(data), Conflict: driveapi.ConflictFail, Fingerprint: "fp-p"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("finished while paused: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	eng.Resume()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = start
	// 192 KiB at 256 KiB/s from an empty bucket takes about 0.75 s
	slow := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	slow.SetBandwidth(256<<10, 0)
	small := payload(192<<10, 7)
	t0 := time.Now()
	if _, err := slow.Upload(ctxT(t), transfer.UploadRequest{ParentID: fd.RootID(), Name: "slow.bin", Size: int64(len(small)),
		Src: bytes.NewReader(small), Conflict: driveapi.ConflictFail}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(t0); el < 550*time.Millisecond {
		t.Fatalf("bandwidth limit not applied: %v", el)
	}
}
