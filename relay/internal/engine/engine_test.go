package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/fakedrive"
	"olsyn-drive/internal/state"
)

type fixture struct {
	t     *testing.T
	d     *fakedrive.Drive
	src   string
	store *state.Store
	dest  string
}

func newFixture(t *testing.T) *fixture {
	d := fakedrive.New()
	t.Cleanup(d.Close)
	st, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, d: d, src: t.TempDir(), store: st, dest: d.Folder("Projects")}
}

func (f *fixture) engine(crash func(string) bool) *Engine {
	api, err := driveapi.New(f.d.URL, "odt_EXAMPLE", "test")
	if err != nil {
		f.t.Fatal(err)
	}
	return &Engine{API: api, Store: f.store, Crash: crash, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Tries: 6}
}

func (f *fixture) write(rel string, data []byte) {
	p := filepath.Join(f.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) items() []Item {
	var out []Item
	filepath.WalkDir(f.src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, _ := d.Info()
		rel, _ := filepath.Rel(f.src, p)
		out = append(out, Item{Abs: p, Rel: filepath.ToSlash(rel), Size: info.Size(), MtimeNs: info.ModTime().UnixNano()})
		return nil
	})
	return out
}

// run sends every source file and returns outcome → rel paths.
func (f *fixture) run(e *Engine, opt RunOptions) (map[string][]string, error) {
	opt.Dest, opt.KeyScope = f.dest, "test"
	var mu sync.Mutex
	got := map[string][]string{}
	opt.OnResult = func(it Item, r Result) {
		mu.Lock()
		got[r.Outcome] = append(got[r.Outcome], it.Rel+": "+r.Reason)
		mu.Unlock()
	}
	run := e.Start(context.Background(), opt)
	for _, it := range f.items() {
		run.Add(it)
	}
	return got, run.Wait()
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestCrashResumeAtEveryStep(t *testing.T) {
	points := []string{"started", "recorded", "part", "sent", "completing", "completed"}
	for _, size := range []int{1500, 5000} { // single PUT and multipart (1 KiB parts)
		for _, point := range points {
			if size <= 2048 && point == "part" {
				continue
			}
			t.Run(point, func(t *testing.T) {
				f := newFixture(t)
				data := random(size)
				f.write("Site/Level 3/model.ifc", data)
				var crashed atomic.Bool
				e := f.engine(func(p string) bool { return p == point && crashed.CompareAndSwap(false, true) })
				got, _ := f.run(e, RunOptions{})
				if !crashed.Load() || len(got[Sent]) != 0 {
					t.Fatalf("no crash at %s: %v", point, got)
				}
				// "Restart": a new engine on the same state.
				got, err := f.run(f.engine(nil), RunOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(got[Sent])+len(got[Already]) != 1 {
					t.Fatalf("after restart at %s: %v", point, got)
				}
				files := f.d.Files()
				if len(files) != 1 || !bytes.Equal(files["Projects/Site/Level 3/model.ifc"].Data, data) {
					t.Fatalf("drive has %v", keys(files))
				}
				if files["Projects/Site/Level 3/model.ifc"].Versions != 1 {
					t.Fatal("a second version was made")
				}
				// A third run finds it there.
				got, _ = f.run(f.engine(nil), RunOptions{})
				if len(got[Already]) != 1 {
					t.Fatalf("re-run: %v", got)
				}
			})
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestResumeSendsOnlyMissingParts(t *testing.T) {
	f := newFixture(t)
	data := random(20 << 10) // 20 parts
	f.write("big.bin", data)
	var crashed atomic.Bool
	e := f.engine(func(p string) bool { return p == "part" && crashed.CompareAndSwap(false, true) })
	e.PartsInFlight = 1
	f.run(e, RunOptions{})
	before := f.d.Puts
	if before == 0 || before >= 20 {
		t.Fatalf("puts before restart: %d", before)
	}
	got, _ := f.run(f.engine(nil), RunOptions{})
	if len(got[Sent]) != 1 {
		t.Fatalf("%v", got)
	}
	if total := f.d.Puts; total != 20 {
		t.Fatalf("resume re-sent parts: %d puts for 20 parts", total)
	}
}

func TestVerifyAndChecksumMismatch(t *testing.T) {
	f := newFixture(t)
	f.d.HashLater = true
	f.write("a.txt", []byte("alpha"))
	e := f.engine(nil)
	got, _ := f.run(e, RunOptions{Verify: true})
	if len(got[Pending]) != 1 {
		t.Fatalf("before hashing: %v", got)
	}
	recs, _ := f.store.All()
	f.d.HashAll()
	done, pending, _, err := e.Verify(context.Background(), recs[0])
	if err != nil || !done || pending {
		t.Fatalf("verify: %v %v %v", done, pending, err)
	}

	g := newFixture(t)
	g.d.Corrupt = true
	g.write("b.txt", []byte("bravo"))
	got, _ = g.run(g.engine(nil), RunOptions{Verify: true})
	if len(got[Held]) != 1 || !strings.Contains(got[Held][0], "checksum") {
		t.Fatalf("corrupt: %v", got)
	}
	// And a spool re-run doesn't treat the fingerprint match as proof.
	got, _ = g.run(g.engine(nil), RunOptions{Verify: true})
	if len(got[Already]) != 0 {
		t.Fatalf("fingerprint taken as proof: %v", got)
	}
}

func TestConflictPolicies(t *testing.T) {
	for _, tc := range []struct {
		policy, outcome string
		versions        int
		files           int
	}{{OnConflictHold, Held, 1, 1}, {OnConflictNewVersion, Sent, 2, 1}, {OnConflictKeepBoth, Sent, 1, 2}} {
		t.Run(tc.policy, func(t *testing.T) {
			f := newFixture(t)
			f.d.Put(f.dest, "plan.pdf", []byte("someone else's plan"))
			f.write("plan.pdf", []byte("my plan"))
			got, _ := f.run(f.engine(nil), RunOptions{Conflict: tc.policy})
			if len(got[tc.outcome]) != 1 {
				t.Fatalf("%v", got)
			}
			files := f.d.Files()
			if len(files) != tc.files || files["Projects/plan.pdf"].Versions != tc.versions {
				t.Fatalf("files %v versions %d", keys(files), files["Projects/plan.pdf"].Versions)
			}
		})
	}
	// Same content uploaded another way: already there, not a conflict.
	f := newFixture(t)
	f.d.Put(f.dest, "same.pdf", []byte("identical"))
	f.write("same.pdf", []byte("identical"))
	got, _ := f.run(f.engine(nil), RunOptions{})
	if len(got[Already]) != 1 {
		t.Fatalf("identical: %v", got)
	}
}

func TestRetriesThroughDriveRestart(t *testing.T) {
	f := newFixture(t)
	var n atomic.Int32
	f.d.Fail = func(r *http.Request) int {
		if strings.HasPrefix(r.URL.Path, "/api/") && n.Add(1)%3 != 0 {
			return 503 // a deploy: two of every three API calls fail
		}
		return 0
	}
	for i := range 5 {
		f.write(filepath.Join("d", string(rune('a'+i))+".bin"), random(3000))
	}
	got, err := f.run(f.engine(nil), RunOptions{Files: 3})
	if err != nil || len(got[Sent]) != 5 {
		t.Fatalf("%v %v", got, err)
	}
	if len(f.d.Files()) != 5 || f.d.Pending() != 0 {
		t.Fatalf("files %d pending %d", len(f.d.Files()), f.d.Pending())
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.d.Put(f.dest, "there.txt", []byte("x"))
	f.write("there.txt", []byte("x"))
	f.write("new/deep/file.txt", []byte("y"))
	f.write("clash.txt", []byte("mine"))
	f.d.Put(f.dest, "clash.txt", []byte("theirs"))
	got, _ := f.run(f.engine(nil), RunOptions{DryRun: true})
	if len(got[Already]) != 1 || len(got[WouldSend]) != 1 || len(got[Held]) != 1 {
		t.Fatalf("%v", got)
	}
	if f.d.Starts != 0 || len(f.d.Files()) != 2 {
		t.Fatal("dry run changed the drive")
	}
	if recs, _ := f.store.All(); len(recs) != 0 {
		t.Fatalf("dry run wrote %d records", len(recs))
	}
}

func TestChangedWhileSending(t *testing.T) {
	f := newFixture(t)
	f.write("grow.bin", random(5000))
	it := f.items()[0]
	e := f.engine(func(p string) bool {
		if p == "recorded" {
			// The file grows after it was listed.
			fh, _ := os.OpenFile(it.Abs, os.O_APPEND|os.O_WRONLY, 0)
			fh.Write([]byte("more"))
			fh.Close()
		}
		return false
	})
	opt := RunOptions{Dest: f.dest, KeyScope: "test"}
	var res Result
	opt.OnResult = func(_ Item, r Result) { res = r }
	run := e.Start(context.Background(), opt)
	run.Add(it)
	run.Wait()
	if res.Outcome != Failed || !strings.Contains(res.Reason, "changed") {
		t.Fatalf("%+v", res)
	}
	if len(f.d.Files()) != 0 {
		t.Fatal("published a file that changed while it was read")
	}
}
