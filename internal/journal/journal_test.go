package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAppendUpdateReplayDone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := j.Append(Op{Kind: OpMkdir, NodeID: "l-1", Name: "Docs", IsDir: true})
	b, _ := j.Append(Op{Kind: OpUpload, NodeID: "l-2", ParentID: "l-1", Name: "a.txt", ContentKey: "local-1", Size: 5})
	c, _ := j.Append(Op{Kind: OpRename, NodeID: "l-2", Name: "b.txt"})
	if !(a.Seq < b.Seq && b.Seq < c.Seq) {
		t.Fatalf("sequence not increasing: %d %d %d", a.Seq, b.Seq, c.Seq)
	}
	b.UploadID = "up-1"
	if err := j.Update(b); err != nil {
		t.Fatal(err)
	}
	if err := j.Done(a.Seq); err != nil {
		t.Fatal(err)
	}
	j.Close()

	// replay after a restart: order and upload state are kept
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ops, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Seq != b.Seq || ops[0].UploadID != "up-1" || ops[1].Kind != OpRename || ops[0].State != StatePending {
		t.Fatalf("replay %+v", ops)
	}
	if err := j.Done(a.Seq); err != nil {
		t.Fatal("done twice must be harmless")
	}
	d, _ := j.Append(Op{Kind: OpDelete, NodeID: "x"})
	if d.Seq <= c.Seq {
		t.Fatal("sequence reused after restart")
	}
	if j.Len() != 3 {
		t.Fatalf("len %d", j.Len())
	}
}

// A written record survives a crash of the process (no Sync, no Close).
func TestWrittenSurvivesProcessCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := j.Write(Op{Kind: OpUpload, NodeID: "l-1", Name: "a.txt", ContentKey: "local-1", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	j.Abandon()
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got, err := j.Get(a.Seq); err != nil || got.ContentKey != "local-1" {
		t.Fatalf("after crash: %+v %v", got, err)
	}
}

// A record cut short by a crash is dropped; the ones before it stay, and
// new records after reopening are not lost behind the torn one.
func TestTornTailIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := j.Append(Op{Kind: OpMkdir, NodeID: fmt.Sprint("l-", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(path)
	rec := encode(recPut, []byte(`{"seq":4,"kind":"mkdir","node":"l-3"}`))
	torn := append(full, rec[:len(rec)-5]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Len() != 3 || j.Stats().TornBytes != int64(len(rec)-5) {
		t.Fatalf("len %d torn %d", j.Len(), j.Stats().TornBytes)
	}
	d, err := j.Append(Op{Kind: OpDelete, NodeID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, err := j.Get(d.Seq); err != nil || j.Len() != 4 {
		t.Fatalf("record after a dropped tail lost: len %d %v", j.Len(), err)
	}
	// a corrupted byte in the middle ends the replay there, too
	b, _ := os.ReadFile(path)
	b[len(full)-3] ^= 0xff
	_ = os.WriteFile(path, b, 0o600)
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if j2.Len() != 2 {
		t.Fatalf("after corruption: %d live", j2.Len())
	}
}

// Many savers at once share fsyncs (group commit); each returns only after
// an fsync that covers its record.
func TestGroupCommitSharesFsyncs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	const savers, each = 16, 20
	t0 := time.Now()
	var wg sync.WaitGroup
	for s := range savers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := j.Append(Op{Kind: OpUpload, NodeID: fmt.Sprintf("l-%d-%d", s, i)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	st := j.Stats()
	t.Logf("%d appends by %d savers: %d fsyncs in %v", savers*each, savers, st.Syncs, time.Since(t0))
	if st.Syncs >= savers*each/2 {
		t.Fatalf("no group commit: %d fsyncs for %d appends", st.Syncs, savers*each)
	}
	// Sync with nothing new written does not fsync
	before := j.Stats().Syncs
	if err := j.Sync(context.Background()); err != nil || j.Stats().Syncs != before {
		t.Fatalf("idle sync fsynced: %v", err)
	}
}

// Compaction keeps the live operations, their order and the sequence.
func TestCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pad := string(make([]byte, 2000))
	var keep []Op
	for i := range 4000 {
		op, err := j.Write(Op{Kind: OpUpload, NodeID: fmt.Sprint("l-", i), Reason: pad})
		if err != nil {
			t.Fatal(err)
		}
		if i%100 == 0 {
			keep = append(keep, op)
			continue
		}
		if err := j.Done(op.Seq); err != nil {
			t.Fatal(err)
		}
	}
	st := j.Stats()
	if st.Compactions == 0 || st.Size > 4<<20 {
		t.Fatalf("not compacted: %+v", st)
	}
	last, _ := j.Write(Op{Kind: OpDelete, NodeID: "z"})
	_ = j.Close()
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ops, _ := j.List()
	if len(ops) != len(keep)+1 || ops[0].Seq != keep[0].Seq || ops[len(ops)-1].Seq != last.Seq {
		t.Fatalf("after compaction: %d ops", len(ops))
	}
	n, _ := j.Write(Op{Kind: OpDelete, NodeID: "y"})
	if n.Seq <= last.Seq {
		t.Fatal("sequence reused after compaction")
	}
}
