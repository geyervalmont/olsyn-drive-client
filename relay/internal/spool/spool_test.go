package spool

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/engine"
	"olsyn-drive/internal/fakedrive"
	"olsyn-drive/internal/state"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fixture struct {
	t     *testing.T
	d     *fakedrive.Drive
	s     *Spool
	clk   *clock
	root  string
	dest  string
	store *state.Store
}

func newFixture(t *testing.T, cfg Config, conflict string) *fixture {
	d := fakedrive.New()
	t.Cleanup(d.Close)
	dest := d.Folder("Projects/Example Tower")
	d.Mode = "shared"
	d.Drops = []map[string]any{{"id": "drop-1", "person": map[string]string{"id": "person-1", "name": "Alex Example"},
		"folder_id": dest, "label": "Plans", "on_conflict": conflict, "person_folder": "Alex Example", "path": "Plans"}}
	api, err := driveapi.New(d.URL, "odt_EXAMPLE", "test")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := state.Open(filepath.Join(t.TempDir(), "state"))
	cfg.Root = filepath.Join(t.TempDir(), "Uploads")
	cfg.Settle, cfg.ZeroSettle = 20*time.Second, 120*time.Second
	if cfg.FreeFloor == 0 {
		cfg.FreeFloor = 1 // the test machine's disk may be nearly full
	}
	s := New(cfg, api, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := s.Engine()
	e.MinBackoff, e.MaxBackoff, e.Tries = time.Millisecond, 5*time.Millisecond, 4
	clk := &clock{t: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	s.now = clk.now
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.stopAll)
	return &fixture{t: t, d: d, s: s, clk: clk, root: cfg.Root, dest: dest, store: st}
}

func (f *fixture) write(rel string, data []byte) string {
	p := filepath.Join(f.root, "Alex Example", "Plans", filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// settle scans, lets time pass, and scans until cond holds.
func (f *fixture) until(cond func() bool) {
	f.t.Helper()
	for i := 0; i < 200; i++ {
		f.s.Scan(context.Background())
		if cond() {
			return
		}
		f.clk.add(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("never happened; stats %+v", f.s.Stats())
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestDeliversVerifiesAndDeletes(t *testing.T) {
	f := newFixture(t, Config{}, "hold")
	a := f.write("Level 3/plan.pdf", bytes.Repeat([]byte("p"), 3000))
	b := f.write("notes.txt", []byte("notes"))
	f.write(".DS_Store", []byte("junk"))
	f.write("~$draft.docx", []byte("owner file"))

	// Not before it has settled.
	f.s.Scan(context.Background())
	f.clk.add(10 * time.Second)
	f.s.Scan(context.Background())
	time.Sleep(20 * time.Millisecond)
	if f.d.Starts != 0 {
		t.Fatal("sent before the file settled")
	}
	f.until(func() bool { return !exists(a) && !exists(b) })
	files := f.d.Files()
	if len(files) != 2 || files["Projects/Example Tower/Level 3/plan.pdf"].Person != "person-1" {
		t.Fatalf("drive: %v person %q", keysOf(files), files["Projects/Example Tower/Level 3/plan.pdf"].Person)
	}
	if exists(filepath.Join(f.root, "Alex Example", "Plans", "Level 3")) {
		t.Error("empty folder left behind")
	}
	if !exists(filepath.Join(f.root, "Alex Example", "Plans")) {
		t.Error("the drop folder itself was removed")
	}
	if !exists(filepath.Join(f.root, "Alex Example", "Plans", "~$draft.docx")) {
		t.Error("a temp file was touched")
	}
	if f.s.Stats().SentFiles != 2 {
		t.Errorf("stats %+v", f.s.Stats())
	}
}

func TestGrowingFileWaits(t *testing.T) {
	f := newFixture(t, Config{}, "hold")
	p := f.write("model.ifc", []byte("part 1"))
	for i := 0; i < 6; i++ { // grows every 5 s for 30 s: never settles
		f.s.Scan(context.Background())
		f.clk.add(5 * time.Second)
		fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		fh.Write([]byte(" more"))
		fh.Close()
	}
	time.Sleep(20 * time.Millisecond)
	if f.d.Starts != 0 {
		t.Fatal("sent a file that was still growing")
	}
	f.until(func() bool { return !exists(p) })
	if got := string(f.d.Files()["Projects/Example Tower/model.ifc"].Data); got != "part 1 more more more more more more" {
		t.Fatalf("sent %q", got)
	}
}

func TestHeldStaysAndMismatchKept(t *testing.T) {
	f := newFixture(t, Config{}, "hold")
	f.d.Put(f.dest, "clash.pdf", []byte("someone else's"))
	p := f.write("clash.pdf", []byte("mine"))
	f.until(func() bool { return f.s.Stats().Held == 1 })
	if !exists(p) {
		t.Fatal("a held file was deleted")
	}
	starts := f.d.Starts
	for range 5 {
		f.s.Scan(context.Background())
		f.clk.add(30 * time.Second)
	}
	if f.d.Starts != starts {
		t.Fatal("a held file was retried without changing")
	}

	g := newFixture(t, Config{}, "hold")
	g.d.Corrupt = true
	q := g.write("x.bin", []byte("payload"))
	g.until(func() bool { return g.s.Stats().Held == 1 })
	if !exists(q) {
		t.Fatal("deleted a file whose checksum didn't match")
	}
}

func TestWaitsForDriveHash(t *testing.T) {
	f := newFixture(t, Config{}, "hold")
	f.d.HashLater = true
	p := f.write("a.txt", []byte("alpha"))
	f.until(func() bool { return len(f.d.Files()) == 1 })
	for range 5 {
		f.s.Scan(context.Background())
		f.clk.add(30 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if !exists(p) {
		t.Fatal("deleted before the drive confirmed its copy")
	}
	f.d.HashAll()
	f.until(func() bool { return !exists(p) })
	if f.d.Files()["Projects/Example Tower/a.txt"].Versions != 1 {
		t.Fatal("re-sent while waiting")
	}
}

func TestFullAndRevoked(t *testing.T) {
	f := newFixture(t, Config{MaxBytes: 100}, "hold")
	f.write("big.bin", make([]byte, 200))
	f.s.Scan(context.Background())
	marker := filepath.Join(f.root, "Alex Example", "Plans", FullMarker)
	if !f.s.Stats().Full || !exists(marker) {
		t.Fatalf("not full: %+v", f.s.Stats())
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(f.root, "Alex Example", "Plans")); st.Mode().Perm()&0o022 != 0 {
			t.Fatalf("full drop folder still writable: %v", st.Mode())
		}
	}
	f.until(func() bool { return !f.s.Stats().Full })
	if exists(marker) {
		t.Fatal("marker left after the spool drained")
	}
	if f.d.Files()[FullMarker].Data != nil {
		t.Fatal("the marker was uploaded")
	}

	// A revoked drop: its files stay put and count as unmapped.
	f.d.Drops = nil
	if err := f.s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := f.write("after.txt", []byte("late"))
	for range 10 {
		f.s.Scan(context.Background())
		f.clk.add(30 * time.Second)
	}
	if !exists(p) || f.s.Stats().Unmapped != 1 {
		t.Fatalf("revoked drop: exists %v stats %+v", exists(p), f.s.Stats())
	}
}

func TestRemoveRefusesSwappedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f := newFixture(t, Config{}, "hold")
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep.txt")
	os.WriteFile(victim, []byte("keep"), 0o644)
	p := f.write("sub/keep.txt", []byte("keep"))
	st, _ := os.Stat(p)
	drop := filepath.Join(f.root, "Alex Example", "Plans")
	// After verification, someone swaps the folder for a link out.
	os.RemoveAll(filepath.Join(drop, "sub"))
	os.Symlink(outside, filepath.Join(drop, "sub"))
	os.Chtimes(victim, st.ModTime(), st.ModTime())
	it := itemFor(p, "sub/keep.txt", st)
	if err := f.s.remove(drop, it); err == nil || !exists(victim) {
		t.Fatal("deleted through a swapped-in link")
	}
}

func itemFor(abs, rel string, st os.FileInfo) engine.Item {
	return engine.Item{Abs: abs, Rel: rel, Size: st.Size(), MtimeNs: st.ModTime().UnixNano()}
}

func keysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
