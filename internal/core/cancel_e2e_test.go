package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/core"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func manyFiles(t *testing.T, n int, size int) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "Many")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%02d.bin", i)), bytes.Repeat([]byte{byte(i)}, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func (h *harness) transfers(states string) agent.TransferPage {
	h.t.Helper()
	var page agent.TransferPage
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/transfers?limit=500&state="+states, nil, &page); err != nil {
		h.t.Fatal(err)
	}
	return page
}

func seqOf(id string) int {
	n, _ := strconv.Atoi(id[strings.LastIndex(id, "_")+1:])
	return n
}

// Cancelling a send file that is paused (its upload waiting for resume),
// queued, or found while the queue is full: it leaves the lists at once,
// is recorded as cancelled, and is never sent.
func TestCancelSendWhilePausedQueuedOrBackedUp(t *testing.T) {
	h := start(t)
	h.signIn()
	ctx := context.Background()
	if err := h.client.Do(ctx, http.MethodPost, "/v1/pause", nil, nil); err != nil {
		t.Fatal(err)
	}
	// 4 files at once; the others are found ahead (the pre-check reads
	// ahead to ask about many files at once) and wait for room
	src := manyFiles(t, 14, 150<<10)
	var res agent.SendResult
	if err := h.client.Do(ctx, http.MethodPost, "/v1/send", map[string]any{"local_paths": []string{src}, "drive_folder": "/"}, &res); err != nil {
		t.Fatal(err)
	}
	var live agent.TransferPage
	deadline := time.Now().Add(10 * time.Second)
	for {
		live = h.transfers("active,waiting")
		if live.Counts.Active == 4 && live.Counts.Waiting == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not paused with 4 active and 10 waiting: %+v", live.Counts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var paused, queued, backedUp agent.Transfer
	for _, tr := range live.Items {
		switch {
		case tr.State == agent.TransferActive && paused.ID == "":
			paused = tr
		case tr.State == agent.TransferWaiting && (queued.ID == "" || seqOf(tr.ID) < seqOf(queued.ID)):
			queued = tr
		}
		if tr.State == agent.TransferWaiting && (backedUp.ID == "" || seqOf(tr.ID) > seqOf(backedUp.ID)) {
			backedUp = tr
		}
	}
	for _, tr := range []agent.Transfer{paused, queued, backedUp} {
		var got agent.Transfer
		if err := h.client.Do(ctx, http.MethodPost, "/v1/transfers/"+tr.ID+"/cancel", nil, &got); err != nil {
			t.Fatal(err)
		}
		if got.State != agent.TransferCancelled {
			t.Fatalf("cancel %s: %+v", tr.ID, got)
		}
	}
	// the paused file's place goes to the next one: 14 found, 3 cancelled
	deadline = time.Now().Add(5 * time.Second)
	for {
		c := h.transfers("active,waiting").Counts
		if c.Active == 4 && c.Waiting == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("listed after cancel: %+v", c)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, tr := range h.transfers("active,waiting").Items {
		if tr.ID == paused.ID || tr.ID == queued.ID || tr.ID == backedUp.ID {
			t.Fatalf("%s still listed after cancel", tr.ID)
		}
	}
	cancelled := h.transfers("cancelled")
	if len(cancelled.Items) != 3 || cancelled.Items[0].Error == nil || cancelled.Items[0].Error.Code != "cancelled" {
		t.Fatalf("history %+v", cancelled.Items)
	}
	// a cancelled file cannot be cancelled again
	var ae *agent.APIError
	if err := h.client.Do(ctx, http.MethodPost, "/v1/transfers/"+queued.ID+"/cancel", nil, nil); !errors.As(err, &ae) || ae.Code != "not_cancellable" {
		t.Fatalf("second cancel: %v", err)
	}

	if err := h.client.Do(ctx, http.MethodPost, "/v1/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	b := waitBatch(t, h, res.BatchID, func(b agent.Batch) bool { return b.State == agent.BatchDone })
	if b.FilesDone != 11 || b.Files != 11 {
		t.Fatalf("batch %+v", b)
	}
	for _, tr := range []agent.Transfer{paused, queued, backedUp} {
		if _, ok := h.fd.Lookup("/Many/" + tr.Name); ok {
			t.Fatalf("%s (cancelled) reached the drive", tr.Name)
		}
	}
	if n := h.fd.PendingUploads(); n != 0 {
		t.Fatalf("%d upload sessions left open on the drive", n)
	}
	if c := h.transfers("active,waiting").Counts; c.Active+c.Waiting != 0 {
		t.Fatalf("left in the lists: %+v", c)
	}
	// an unknown batch
	if err := h.client.Do(ctx, http.MethodPost, "/v1/batches/b_99/cancel", nil, nil); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("unknown batch: %v", err)
	}
}

// Cancelling a send file while it waits to try again (the drive answered
// 503): it is not tried again.
func TestCancelSendWaitingToRetry(t *testing.T) {
	h := start(t)
	h.signIn()
	ctx := context.Background()
	if err := h.client.Do(ctx, http.MethodPost, "/v1/pause", nil, nil); err != nil {
		t.Fatal(err)
	}
	src := manyFiles(t, 1, 150<<10)
	var res agent.SendResult
	if err := h.client.Do(ctx, http.MethodPost, "/v1/send", map[string]any{"local_paths": []string{src}, "drive_folder": "/"}, &res); err != nil {
		t.Fatal(err)
	}
	h.fd.Unavailable(time.Minute)
	if err := h.client.Do(ctx, http.MethodPost, "/v1/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	var waiting agent.Transfer
	deadline := time.Now().Add(10 * time.Second)
	for waiting.ID == "" {
		page, _ := h.core.Transfers(ctx, agent.TransferQuery{States: []string{agent.TransferWaiting}})
		for _, tr := range page.Items {
			if tr.State == agent.TransferWaiting && tr.Error != nil && tr.Error.RetryAt != nil {
				waiting = tr
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("never waiting to try again")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := h.core.Cancel(ctx, waiting.ID); err != nil {
		t.Fatal(err)
	}
	h.fd.Unavailable(0)
	b := waitBatch(t, h, res.BatchID, func(b agent.Batch) bool { return b.State == agent.BatchDone })
	time.Sleep(300 * time.Millisecond)
	if _, ok := h.fd.Lookup("/Many/f00.bin"); ok || b.FilesDone != 0 {
		t.Fatalf("sent after cancel: %+v", b)
	}
	if len(h.transfers("cancelled").Items) != 1 {
		t.Fatal("not recorded as cancelled")
	}
}

// Cancelling a whole batch midway: nothing more is sent, uploads under way
// are aborted on the drive, the rest is recorded as cancelled, and the
// batch does not carry on after a restart.
func TestCancelBatchMidway(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{PartSize: 64 << 10, SingleMax: 100 << 10})
	t.Cleanup(fd.Close)
	state, cache := t.TempDir(), t.TempDir()
	sess := agent.Session{ServerURL: fd.URL(), Token: fd.Token(), UserID: "u1", DeviceID: "d1", CacheDir: cache, CacheCapBytes: 1 << 30}
	src := manyFiles(t, 30, 150<<10)
	open := func() *core.FS {
		c, err := core.New(core.Options{StateDir: state, HTTPClient: http.DefaultClient, PollInterval: time.Second, Backoff: fast, DisableStream: true})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range c.Events() {
			}
		}()
		if err := c.Start(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		return c
	}
	onDrive := func() int {
		n := 0
		for i := range 30 {
			if _, ok := fd.Lookup(fmt.Sprintf("/Many/f%02d.bin", i)); ok {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	c := open()
	fd.SetLatency(20 * time.Millisecond)
	res, err := c.Send(ctx, agent.SendRequest{LocalPaths: []string{src}, DriveFolder: "/"})
	if err != nil {
		t.Fatal(err)
	}
	for countDone(c, res.BatchID) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.CancelBatch(ctx, res.BatchID); err != nil {
		t.Fatal(err)
	}
	var b agent.Batch
	deadline := time.Now().Add(10 * time.Second)
	for {
		b = c.Batches()[0]
		if b.State == agent.BatchCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch not cancelled: %+v", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	fd.SetLatency(0)
	sent := onDrive()
	page, err := c.Transfers(ctx, agent.TransferQuery{States: []string{agent.TransferCancelled}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 || sent+len(page.Items) > 30 || b.Files != b.FilesDone || sent < b.FilesDone {
		t.Fatalf("batch %+v, %d on the drive, %d cancelled", b, sent, len(page.Items))
	}
	if live, _ := c.Transfers(ctx, agent.TransferQuery{States: []string{agent.TransferActive, agent.TransferWaiting}}); len(live.Items) != 0 {
		t.Fatalf("still listed: %+v", live.Items)
	}
	time.Sleep(500 * time.Millisecond)
	if n := onDrive(); n != sent {
		t.Fatalf("%d more sent after the cancel", n-sent)
	}
	if n := fd.PendingUploads(); n != 0 {
		t.Fatalf("%d upload sessions left open on the drive", n)
	}
	if _, err := c.CancelBatch(ctx, res.BatchID); !errors.Is(err, agent.ErrNotCancellable) {
		t.Fatalf("cancel a finished batch: %v", err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	c2 := open()
	defer c2.Stop(ctx)
	time.Sleep(time.Second)
	if bs := c2.Batches(); len(bs) != 1 || bs[0].State != agent.BatchCancelled {
		t.Fatalf("after restart %+v", bs)
	}
	if n := onDrive(); n != sent {
		t.Fatalf("the cancelled batch carried on after a restart: %d more", n-sent)
	}
}
