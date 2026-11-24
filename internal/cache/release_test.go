package cache

import (
	"testing"
	"time"
)

// upload writes n bytes as unsent content and commits it as version key.
func upload(t *testing.T, c *Cache, key string, n int, up Upload) {
	t.Helper()
	h, err := c.CreateLocal("local-"+key, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt(data(n, 3), 0); err != nil {
		t.Fatal(err)
	}
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	if err := c.CommitUpload("local-"+key, key, up); err != nil {
		t.Fatal(err)
	}
}

var quick = ReleasePolicy{AfterUpload: time.Minute, KeepOpened: 10 * time.Minute, VerifyWait: time.Hour}

func TestReleaseOnlyVerifiedUploadsAfterGrace(t *testing.T) {
	c := openCache(t, t.TempDir(), 1<<30)
	defer c.Close()
	upload(t, c, "v-ok", 3*bs, Upload{Node: "n1", Check: CheckVerified})
	upload(t, c, "v-claimed", 3*bs, Upload{Node: "n2", Check: CheckClaimed})
	upload(t, c, "v-unknown", 3*bs, Upload{Node: "n3"})
	now := time.Now()

	if n, _ := c.ReleaseUploaded(now, quick, nil); n != 0 {
		t.Fatalf("released %d inside the grace period", n)
	}
	u := c.Usage(now, quick)
	if u.Uploaded != 6*bs || u.Opened != 3*bs || u.Unsent != 0 {
		t.Fatalf("usage in grace %+v", u)
	}
	later := now.Add(2 * time.Minute)
	n, b := c.ReleaseUploaded(later, quick, nil)
	if n != 1 || b != 3*bs || c.Has("v-ok") {
		t.Fatalf("released %d (%d bytes); verified kept %v", n, b, c.Has("v-ok"))
	}
	if !c.Has("v-claimed") || !c.Has("v-unknown") {
		t.Fatal("content the drive has not confirmed was released")
	}
	// verified later through the changes feed: released at the next pass
	if err := c.SetCheck("v-claimed", CheckVerified); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.ReleaseUploaded(later, quick, nil); n != 1 || c.Has("v-claimed") {
		t.Fatalf("verified-later not released (%d)", n)
	}
	// never verified: after VerifyWait it is ordinary cached content
	if u := c.Usage(now.Add(2*time.Hour), quick); u.Uploaded != 0 || u.Opened != 3*bs {
		t.Fatalf("usage after the verify wait %+v", u)
	}
	if n, _ := c.FreeNow(nil); n != 0 || !c.Has("v-unknown") {
		t.Fatal("free-up-space dropped an upload the drive never confirmed")
	}
}

func TestReleaseKeepsOpenReadAndKeptContent(t *testing.T) {
	c := openCache(t, t.TempDir(), 1<<30)
	defer c.Close()
	upload(t, c, "open", bs, Upload{Node: "a", Check: CheckVerified})
	upload(t, c, "read", bs, Upload{Node: "b", Check: CheckVerified})
	upload(t, c, "kept", bs, Upload{Node: "c", Check: CheckVerified})
	h, err := c.Open("open")
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Open("read")
	if err != nil {
		t.Fatal(err)
	}
	r.NoteRead()
	_ = r.Close()
	keep := func(key, node string) bool { return node == "c" }
	later := time.Now().Add(2 * time.Minute)
	if n, _ := c.ReleaseUploaded(later, quick, keep); n != 0 {
		t.Fatalf("released %d: open, recently read and kept must stay", n)
	}
	if n, _ := c.ReleaseUploaded(later.Add(quick.KeepOpened), quick, keep); n != 1 || c.Has("read") {
		t.Fatalf("read long ago not released (%d)", n)
	}
	_ = h.Close()
	if n, _ := c.FreeNow(keep); n != 1 || c.Has("open") || !c.Has("kept") {
		t.Fatalf("free now released %d", n)
	}
}

// The upload record survives a restart (and a crash after the commit), so
// the release still waits for the drive's confirmation.
func TestReleaseStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := openCache(t, dir, 1<<30)
	upload(t, c, "v-claimed", 2*bs, Upload{Node: "n", Check: CheckClaimed})
	upload(t, c, "v-ok", 2*bs, Upload{Node: "m", Check: CheckVerified})
	c.Abandon() // a crash: the commit itself was durable
	c = openCache(t, dir, 1<<30)
	defer c.Close()
	later := time.Now().Add(2 * time.Minute)
	if n, _ := c.ReleaseUploaded(later, quick, nil); n != 1 || c.Has("v-ok") || !c.Has("v-claimed") {
		t.Fatalf("after restart released %d", n)
	}
	if err := c.SetCheck("v-claimed", CheckVerified); err != nil {
		t.Fatal(err)
	}
	c.Abandon()
	c = openCache(t, dir, 1<<30)
	if n, _ := c.ReleaseUploaded(later, quick, nil); n != 1 {
		t.Fatalf("verified state lost in a crash (%d)", n)
	}
	c.Abandon()
	// released content stays gone after a crash
	c = openCache(t, dir, 1<<30)
	if c.Has("v-claimed") || c.Has("v-ok") {
		t.Fatal("released content came back")
	}
}

// Under cap pressure, uploads the drive has not confirmed are evicted last.
func TestEvictionKeepsUnconfirmedUploadsLast(t *testing.T) {
	c := openCache(t, t.TempDir(), 4*bs)
	defer c.Close()
	upload(t, c, "v-claimed", 2*bs, Upload{Check: CheckClaimed})
	h, err := c.Get("read", 2*bs)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Fill(0, data(2*bs, 1)); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	upload(t, c, "v-new", 2*bs, Upload{Check: CheckVerified})
	if !c.Has("v-claimed") || c.Has("read") {
		t.Fatalf("evicted the unconfirmed upload first (claimed %v, read %v)", c.Has("v-claimed"), c.Has("read"))
	}
}
