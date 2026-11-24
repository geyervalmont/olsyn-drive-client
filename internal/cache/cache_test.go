package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const bs = 4096

func openCache(t *testing.T, dir string, capBytes int64) *Cache {
	t.Helper()
	c, err := Open(Options{Dir: dir, Cap: capBytes, BlockSize: bs, BackpressureTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func data(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
}

func TestFillReadMissing(t *testing.T) {
	c := openCache(t, t.TempDir(), 1<<20)
	defer c.Close()
	src := data(3*bs+100, 1)
	h, err := c.Get("v1", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if m := h.Missing(10, 2*bs); len(m) != 1 || m[0].Off != 0 || m[0].Len != 3*bs {
		t.Fatalf("missing %+v", m)
	}
	buf := make([]byte, 100)
	if _, err := h.ReadAt(buf, 10); !errors.Is(err, ErrMissing) {
		t.Fatalf("read missing: %v", err)
	}
	if err := h.Fill(bs, src[bs:2*bs]); err != nil {
		t.Fatal(err)
	}
	if m := h.Missing(0, int64(len(src))); len(m) != 2 || m[0].Off != 0 || m[1].Off != 2*bs || m[1].Len != bs+100 {
		t.Fatalf("missing after fill %+v", m)
	}
	n, err := h.ReadAt(buf, bs+5)
	if err != nil || n != 100 || !bytes.Equal(buf, src[bs+5:bs+105]) {
		t.Fatalf("read %d %v", n, err)
	}
	_ = h.Fill(0, src[:bs])
	_ = h.Fill(2*bs, src[2*bs:])
	if !h.Complete() || !c.IsComplete("v1") {
		t.Fatal("should be complete")
	}
	tail := make([]byte, 200)
	n, err = h.ReadAt(tail, int64(len(src))-50)
	if n != 50 || err == nil {
		t.Fatalf("tail read %d %v", n, err)
	}
	h.Close()
}

func TestLocalWriteOnBaseTruncateCommit(t *testing.T) {
	dir := t.TempDir()
	c := openCache(t, dir, 1<<20)
	src := data(3*bs, 2)
	base, _ := c.Get("v1", int64(len(src)))
	_ = base.Fill(0, src[:bs]) // only block 0 cached
	base.Close()

	h, err := c.CreateLocal("l1", "v1", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	// a write that partly covers block 1 needs block 1 from the base first
	m := h.MissingForWrite(bs+10, 20)
	if len(m) != 1 || m[0].Off != bs || m[0].Len != bs {
		t.Fatalf("missing for write %+v", m)
	}
	// a write covering block 2 entirely needs nothing
	if m := h.MissingForWrite(2*bs, bs); len(m) != 0 {
		t.Fatalf("full block write needs %+v", m)
	}
	_ = h.Fill(bs, src[bs:2*bs])
	if _, err := h.WriteAt([]byte("HELLO"), bs+10); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteAt(bytes.Repeat([]byte{'z'}, bs), 2*bs); err != nil {
		t.Fatal(err)
	}
	if !h.Complete() {
		t.Fatalf("missing %+v", h.Missing(0, h.Size()))
	}
	// grow past the end, then shrink
	if _, err := h.WriteAt([]byte("END"), 4*bs); err != nil {
		t.Fatal(err)
	}
	if h.Size() != 4*bs+3 {
		t.Fatalf("size %d", h.Size())
	}
	got := make([]byte, 10)
	if n, err := h.ReadAt(got, 3*bs+5); err != nil || n != 10 || !bytes.Equal(got, make([]byte, 10)) {
		t.Fatalf("gap should read zeros: %v %v", got, err)
	}
	if err := h.Truncate(bs + 15); err != nil {
		t.Fatal(err)
	}
	all := make([]byte, bs+15)
	if _, err := h.ReadAt(all, 0); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, src[:bs+10]...), []byte("HELLO")...)
	if !bytes.Equal(all, want) {
		t.Fatal("content after truncate differs")
	}
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if s := c.Stats(); s.Unsent != bs+15 {
		t.Fatalf("unsent %d", s.Unsent)
	}
	if err := c.Commit("l1", "v2"); err != nil {
		t.Fatal(err)
	}
	if !c.IsComplete("v2") || c.Has("l1") || c.Stats().Unsent != 0 {
		t.Fatal("commit did not make a clean complete entry")
	}
	c.Close()
	// reopen: committed content survives
	c = openCache(t, dir, 1<<20)
	defer c.Close()
	h2, err := c.Open("v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h2.ReadAt(all, 0); err != nil || !bytes.Equal(all, want) {
		t.Fatalf("after reopen: %v", err)
	}
	h2.Close()
}

func TestDirtySurvivesCrashAndOrphansGo(t *testing.T) {
	dir := t.TempDir()
	c := openCache(t, dir, 1<<20)
	h, _ := c.CreateLocal("l1", "", 0)
	_, _ = h.WriteAt([]byte("unsent"), 0)
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	h2, _ := c.CreateLocal("l2", "", 0)
	_, _ = h2.WriteAt([]byte("never synced"), 0)
	// "crash": close the db without closing handles or flushing
	_ = c.db.Close()
	c2 := openCache(t, dir, 1<<20)
	defer c2.Close()
	if keys := c2.DirtyKeys(); len(keys) != 1 || keys[0] != "l1" {
		t.Fatalf("dirty keys %v", keys)
	}
	// l2 was never synced: its file is an orphan; adopt fails for a size it
	// does not hold, then the orphan is dropped
	if err := c2.Adopt("l2", 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adopt of a short file: %v", err)
	}
	c2.DropOrphans()
	files, _ := os.ReadDir(filepath.Join(dir, "data"))
	if len(files) != 1 {
		t.Fatalf("orphan data files left: %d", len(files))
	}
	r, _ := c2.Open("l1")
	buf := make([]byte, 6)
	if _, err := r.ReadAt(buf, 0); err != nil || string(buf) != "unsent" {
		t.Fatalf("read %q %v", buf, err)
	}
	r.Close()
}

func TestAdoptAfterCrash(t *testing.T) {
	dir := t.TempDir()
	c := openCache(t, dir, 1<<20)
	h, _ := c.CreateLocal("local-1", "", 0)
	_, _ = h.WriteAt([]byte("journalled but metadata lost"), 0)
	if !h.Adoptable() {
		t.Fatal("new content should be adoptable")
	}
	if err := h.SyncData(); err != nil {
		t.Fatal(err)
	}
	c.Abandon()
	c = openCache(t, dir, 1<<20)
	defer c.Close()
	if c.Has("local-1") {
		t.Fatal("metadata was never persisted, entry should be unknown")
	}
	if err := c.Adopt("local-1", 28); err != nil {
		t.Fatal(err)
	}
	r, err := c.Open("local-1")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 28)
	if _, err := r.ReadAt(buf, 0); err != nil || string(buf) != "journalled but metadata lost" {
		t.Fatalf("adopted %q %v", buf, err)
	}
	r.Close()
}

func TestEvictionNeverTakesOpenOrUnsent(t *testing.T) {
	c := openCache(t, t.TempDir(), 4*bs)
	defer c.Close()
	fill := func(key string, seed byte) *Handle {
		h, _ := c.Get(key, 2*bs)
		_ = h.Fill(0, data(2*bs, seed))
		return h
	}
	open := fill("open", 1) // stays open
	a := fill("a", 2)
	a.Close()
	time.Sleep(2 * time.Millisecond)
	u, _ := c.CreateLocal("unsent", "", 0)
	_, _ = u.WriteAt(data(2*bs, 3), 0)
	_ = u.Sync()
	u.Close()
	b := fill("b", 4)
	b.Close()
	c.Evict()
	if !c.Has("open") || !c.Has("unsent") {
		t.Fatal("evicted open or unsent data")
	}
	if c.Has("a") {
		t.Fatal("oldest clean entry should go first")
	}
	open.Close()
	c.SetCap(2 * bs)
	if !c.Has("unsent") {
		t.Fatal("unsent data evicted")
	}
	if s := c.Stats(); s.Used > 2*bs && s.Used != s.Unsent {
		t.Fatalf("used %d unsent %d", s.Used, s.Unsent)
	}
}

func TestBackpressure(t *testing.T) {
	c := openCache(t, t.TempDir(), 2*bs)
	defer c.Close()
	u, _ := c.CreateLocal("q1", "", 0)
	_, _ = u.WriteAt(data(2*bs, 1), 0)
	_ = u.Sync()
	// while the file is still open, its data never blocks other writes
	if err := c.WaitForRoom(context.Background(), bs); err != nil {
		t.Fatalf("open data must not block: %v", err)
	}
	u.Close() // now queued for upload: further writes wait for it to drain
	if err := c.WaitForRoom(context.Background(), bs); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("want ErrNoSpace after timeout, got %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitForRoom(context.Background(), bs) }()
	time.Sleep(50 * time.Millisecond)
	if err := c.Commit("q1", "v1"); err != nil { // upload finished: clean, evictable
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer not released after the upload drained")
	}
}
