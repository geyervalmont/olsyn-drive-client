package vfs_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"
)

// The write path does cache writes only: writing 1 GiB in 64 KiB chunks
// (16,384 writes, as Explorer copies) runs no journal record and no index
// or cache-metadata transaction per write. The writes wrap over a 32 MiB
// window so the test file stays small; the bytes written are 1 GiB.
func TestWritePathDoesNoPerWriteDatabaseWork(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	ctx := ctxT(t)
	const (
		chunk  = 64 << 10
		total  = 1 << 30
		window = 32 << 20
		writes = total / chunk
	)
	fl := rw
	fl.Create, fl.Truncate = true, true
	h, err := e.fs.Open(ctx, "/big-write.bin", fl)
	if err != nil {
		t.Fatal(err)
	}

	upd0, views0 := e.idx.TxCounts()
	jr0 := e.jr.Stats().Writes
	meta0 := e.cache.Stats().MetaWrites
	buf := make([]byte, chunk)
	start := time.Now()
	for i := 0; i < writes; i++ {
		binary.LittleEndian.PutUint64(buf, uint64(i)) // each chunk differs
		if _, err := h.WriteAt(ctx, buf, int64(i*chunk%window)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	upd1, views1 := e.idx.TxCounts()
	jr1 := e.jr.Stats().Writes
	meta1 := e.cache.Stats().MetaWrites
	t.Logf("%d writes of 64 KiB in %v (%.0f MiB/s): index %d updates, %d views; journal %d records; cache metadata %d transactions",
		writes, elapsed.Round(time.Millisecond), float64(total)/(1<<20)/elapsed.Seconds(), upd1-upd0, views1-views0, jr1-jr0, meta1-meta0)

	// The background change poll (every 200 ms here) may run a few
	// transactions meanwhile; anything that grows with the writes would be
	// thousands.
	bound := uint64(8 + 4*elapsed/(200*time.Millisecond))
	if n := (upd1 - upd0) + (views1 - views0) + (meta1 - meta0); n > bound {
		t.Fatalf("%d database transactions during %d writes; want at most %d", n, writes, bound)
	}
	if jr1 != jr0 {
		t.Fatalf("%d journal records during the writes; want none", jr1-jr0)
	}

	// Closing commits once: the file is uploaded with the last pass's bytes.
	if err := h.Cleanup(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, window)
	for i := writes - window/chunk; i < writes; i++ {
		binary.LittleEndian.PutUint64(want[i*chunk%window:], uint64(i))
	}
	e.waitSynced(60 * time.Second)
	fi, ok := e.fd.Lookup("/big-write.bin")
	sum := sha256.Sum256(want)
	if !ok || len(fi.Data) != window || fi.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("on the drive: found %v, %d bytes, sha256 %s", ok, len(fi.Data), fi.SHA256)
	}
}
