package vfs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
)

// TestStorm runs a seeded, randomised storm of creates, overwrites,
// renames, moves, deletes and new, renamed, moved and deleted folders
// against a fake drive whose answers come late (so uploads, renames and
// deletes overlap each other), waits for the queue to drain, and checks
// that the mounted copy, the drive and the model of what was done agree
// file for file.
//
// A few seeds run by default. More:
//
//	OLSYN_STORM_SEEDS=200 go test ./internal/vfs -run TestStorm   (seeds 1..200)
//	OLSYN_STORM_SEED=17   go test ./internal/vfs -run TestStorm   (one seed)
//	OLSYN_STORM_OPS=1000  (operations per seed, default 300)
func TestStorm(t *testing.T) {
	seeds := []uint64{1, 2, 3}
	if s := os.Getenv("OLSYN_STORM_SEEDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("OLSYN_STORM_SEEDS=%q: want a count", s)
		}
		seeds = seeds[:0]
		for i := 1; i <= n; i++ {
			seeds = append(seeds, uint64(i))
		}
	}
	if s := os.Getenv("OLSYN_STORM_SEED"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("OLSYN_STORM_SEED=%q: want a number", s)
		}
		seeds = []uint64{n}
	}
	ops := 300
	if s := os.Getenv("OLSYN_STORM_OPS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			t.Fatalf("OLSYN_STORM_OPS=%q: want a count", s)
		}
		ops = n
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { runStorm(t, seed, ops) })
	}
}

// TestDeleteWhileUploadCompletes deletes a new file while its upload's
// complete call is on its way, and lets the drive finish that call after
// the client has given up on it and before the client's abort arrives: the
// drive then holds a finished file the client never heard about, which
// must go to the trash (the robustness run's "only on the drive" files).
func TestDeleteWhileUploadCompletes(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	var held atomic.Bool
	seen, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete") && held.CompareAndSwap(false, true):
			close(seen)
			<-release
			// the drive finishes what it started, whether the client waits or not
			time.AfterFunc(100*time.Millisecond, func() { close(completed) })
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/uploads/") && held.Load():
			select {
			case <-completed:
			case <-time.After(5 * time.Second):
			}
		}
		return false
	})
	if err := e.writeFile("/late.bin", payload(10<<10, 3), time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never reached its complete call")
	}
	if err := e.fs.Remove(ctxT(t), "/late.bin"); err != nil {
		t.Fatal(err)
	}
	close(release)
	e.waitSynced(20 * time.Second)
	if fi, ok := e.fd.Lookup("/late.bin"); ok && !fi.Trashed {
		t.Fatal("the deleted file is on the drive")
	}
	if _, err := e.fs.Stat(ctxT(t), "/late.bin"); vfs.KindOf(err) != vfs.KindNotFound {
		t.Fatalf("stat after delete: %v", err)
	}
}

// storm is the model: what the mounted copy should hold.
type storm struct {
	t       *testing.T
	e       *env
	rng     *rand.Rand
	n       int
	folders []string          // folder paths below the storm's top ("" is the top)
	files   []string          // file paths, in the order they were made
	content map[string][]byte // file path -> content
	counts  map[string]int
	history []string // what was done, for the report of a difference
}

func (s *storm) did(format string, args ...any) {
	s.history = append(s.history, fmt.Sprintf("%d: ", s.n)+fmt.Sprintf(format, args...))
}

const stormTop = "/storm"

func runStorm(t *testing.T, seed uint64, ops int) {
	e := newEnvWith(t, fakedrive.Options{PartSize: part, SingleMax: 100 << 10})
	e.fd.SetJitter(seed, 4*time.Millisecond)
	e.start()
	defer e.stop()
	e.mkdir(stormTop)
	s := &storm{t: t, e: e, rng: rand.New(rand.NewPCG(seed, 0x5eed)), folders: []string{""},
		content: map[string][]byte{}, counts: map[string]int{}}
	for i := 0; i < ops; i++ {
		s.step()
		if s.rng.IntN(4) == 0 {
			// let some of what is queued get going before the next change
			time.Sleep(time.Duration(s.rng.IntN(3000)) * time.Microsecond)
		}
	}
	t.Logf("seed %d: %v", seed, s.counts)
	if os.Getenv("OLSYN_STORM_HISTORY") == "all" {
		t.Logf("history:\n%s", strings.Join(s.history, "\n"))
	}
	waitFor(t, 120*time.Second, "the storm to settle", func() bool { return e.fs.Pending() == 0 })
	if held := e.fs.Held(); len(held) > 0 {
		for _, h := range held {
			t.Errorf("held: %s %s: %s %s", h.Kind, h.Path, h.Code, h.Reason)
		}
	}
	s.compare("after the queue drained")
	// the changes feed must not bring anything back (or take anything away)
	e.fs.Refresh()
	time.Sleep(600 * time.Millisecond)
	waitFor(t, 30*time.Second, "the queue to stay drained", func() bool { return e.fs.Pending() == 0 })
	s.compare("after the changes feed caught up")
}

func (s *storm) abs(rel string) string {
	if rel == "" {
		return stormTop
	}
	return stormTop + "/" + rel
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (s *storm) data() []byte {
	size := s.rng.IntN(16 << 10)
	if s.rng.IntN(5) == 0 {
		size = s.rng.IntN(3*part) + 1 // some multipart uploads
	}
	return payload(size, byte(s.rng.IntN(256)))
}

func (s *storm) pickFile() (int, string, bool) {
	if len(s.files) == 0 {
		return 0, "", false
	}
	i := s.rng.IntN(len(s.files))
	return i, s.files[i], true
}

func (s *storm) pickFolder(withTop bool) (string, bool) {
	if withTop {
		return s.folders[s.rng.IntN(len(s.folders))], true
	}
	if len(s.folders) < 2 {
		return "", false
	}
	return s.folders[1+s.rng.IntN(len(s.folders)-1)], true
}

func (s *storm) must(what string, err error) {
	s.t.Helper()
	if err != nil {
		s.fatalf("%s: %v", what, err)
	}
}

// fatalf stops the run; with OLSYN_STORM_HISTORY set it first shows what
// was done.
func (s *storm) fatalf(format string, args ...any) {
	s.t.Helper()
	if os.Getenv("OLSYN_STORM_HISTORY") != "" {
		s.t.Logf("history:\n%s", strings.Join(s.history, "\n"))
		var paths []string
		for p, it := range s.mounted() {
			if it.dir {
				p += "/"
			}
			paths = append(paths, p)
		}
		sort.Strings(paths)
		s.t.Logf("mounted copy now:\n%s", strings.Join(paths, "\n"))
	}
	s.t.Fatalf(format, args...)
}

func (s *storm) step() {
	s.n++
	roll := s.rng.IntN(100)
	switch {
	case roll < 10:
		s.mkdir()
	case roll < 38 || len(s.files) < 5:
		s.create()
	case roll < 54:
		s.overwrite()
	case roll < 65:
		s.rename()
	case roll < 71:
		s.move()
	case roll < 81:
		s.delete()
	case roll < 85:
		// delete, then a new file of the same name at once
		if _, p, ok := s.pickFile(); ok {
			s.deletePath(p)
			s.write(p, "recreate")
		}
	case roll < 88:
		// rename, then delete the renamed file
		if i, _, ok := s.pickFile(); ok {
			s.renameAt(i)
			s.deletePath(s.files[i])
		}
	case roll < 91:
		// overwrite, then rename
		if i, p, ok := s.pickFile(); ok {
			s.write(p, "overwrite")
			s.renameAt(i)
		}
	case roll < 95:
		s.renameFolder(false)
	case roll < 97:
		s.renameFolder(true)
	default:
		s.deleteFolder()
	}
}

func (s *storm) mkdir() {
	parent, _ := s.pickFolder(true)
	rel := join(parent, fmt.Sprintf("dir-%d", s.n))
	s.e.mkdir(s.abs(rel))
	s.did("mkdir %s", rel)
	s.folders = append(s.folders, rel)
	s.counts["mkdir"]++
}

func (s *storm) create() {
	parent, _ := s.pickFolder(true)
	s.write(join(parent, fmt.Sprintf("file-%d.dat", s.n)), "create")
}

func (s *storm) write(rel, what string) {
	data := s.data()
	s.must(what+" "+rel, s.e.writeFile(s.abs(rel), data, time.Time{}))
	s.did("%s %s (%d bytes)", what, rel, len(data))
	if _, ok := s.content[rel]; !ok {
		s.files = append(s.files, rel)
	}
	s.content[rel] = data
	s.counts[what]++
}

func (s *storm) overwrite() {
	if _, p, ok := s.pickFile(); ok {
		s.write(p, "overwrite")
	}
}

func (s *storm) rename() {
	if i, _, ok := s.pickFile(); ok {
		s.renameAt(i)
	}
}

func (s *storm) renameAt(i int) {
	old := s.files[i]
	rel := join(path.Dir("/" + old)[1:], fmt.Sprintf("renamed-%d.dat", s.n))
	s.moveFile(i, rel, "rename")
}

func (s *storm) move() {
	i, old, ok := s.pickFile()
	if !ok {
		return
	}
	dst, _ := s.pickFolder(true)
	if dst == path.Dir("/" + old)[1:] {
		return
	}
	s.moveFile(i, join(dst, fmt.Sprintf("moved-%d.dat", s.n)), "move")
}

func (s *storm) moveFile(i int, rel, what string) {
	old := s.files[i]
	s.must(what+" "+old, s.e.fs.Rename(ctxT(s.t), s.abs(old), s.abs(rel), false))
	s.did("%s %s -> %s", what, old, rel)
	s.content[rel] = s.content[old]
	delete(s.content, old)
	s.files[i] = rel
	s.counts[what]++
}

func (s *storm) delete() {
	if _, p, ok := s.pickFile(); ok {
		s.deletePath(p)
	}
}

// deletePath deletes a file, half the time the way Windows does (open,
// mark delete-on-close, close).
func (s *storm) deletePath(rel string) {
	ctx := ctxT(s.t)
	if s.rng.IntN(2) == 0 {
		s.must("delete "+rel, s.e.fs.Remove(ctx, s.abs(rel)))
	} else {
		h, err := s.e.fs.Open(ctx, s.abs(rel), vfs.OpenFlags{Access: vfs.AccessRead | vfs.AccessDelete, Share: vfs.ShareAll})
		s.must("open to delete "+rel, err)
		s.must("mark "+rel, h.SetDeleteOnClose(ctx, true))
		s.must("clean up "+rel, h.Cleanup(ctx, false))
		s.must("close "+rel, h.Close())
	}
	s.did("delete %s", rel)
	delete(s.content, rel)
	s.files = slices.DeleteFunc(s.files, func(p string) bool { return p == rel })
	s.counts["delete"]++
}

// renameFolder renames a folder in place, or moves it into another folder
// (not into itself), with whatever is inside and still going up.
func (s *storm) renameFolder(move bool) {
	old, ok := s.pickFolder(false)
	if !ok {
		return
	}
	parent := path.Dir("/" + old)[1:]
	what := "folder rename"
	if move {
		dst, _ := s.pickFolder(true)
		if dst == parent || dst == old || strings.HasPrefix(dst, old+"/") {
			return
		}
		parent, what = dst, "folder move"
	}
	rel := join(parent, fmt.Sprintf("moved-dir-%d", s.n))
	s.must(what+" "+old, s.e.fs.Rename(ctxT(s.t), s.abs(old), s.abs(rel), false))
	s.did("%s %s -> %s", what, old, rel)
	re := func(p string) string {
		if p == old {
			return rel
		}
		if strings.HasPrefix(p, old+"/") {
			return rel + p[len(old):]
		}
		return p
	}
	for i := range s.folders {
		s.folders[i] = re(s.folders[i])
	}
	content := map[string][]byte{}
	for p, d := range s.content {
		content[re(p)] = d
	}
	s.content = content
	for i := range s.files {
		s.files[i] = re(s.files[i])
	}
	s.counts[what]++
}

// deleteFolder deletes a folder and everything in it, deepest first, as
// Explorer does.
func (s *storm) deleteFolder() {
	dir, ok := s.pickFolder(false)
	if !ok {
		return
	}
	inside := func(p string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }
	var files []string
	for _, f := range s.files {
		if inside(f) {
			files = append(files, f)
		}
	}
	for _, f := range files {
		s.deletePath(f)
	}
	var dirs []string
	for _, d := range s.folders {
		if inside(d) {
			dirs = append(dirs, d)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, d := range dirs {
		if err := s.e.fs.Remove(ctxT(s.t), s.abs(d)); err != nil {
			infos, _, _ := s.e.fs.ReadDir(ctxT(s.t), s.abs(d), "", 0)
			var names []string
			for _, in := range infos {
				names = append(names, in.Name)
			}
			s.fatalf("delete folder %s: %v (it holds %v)", d, err, names)
		}
		s.did("delete folder %s", d)
	}
	s.folders = slices.DeleteFunc(s.folders, inside)
	s.counts["folder delete"]++
}

// ---- comparing ----

type treeItem struct {
	dir bool
	sum string
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *storm) want() map[string]treeItem {
	out := map[string]treeItem{}
	for _, d := range s.folders[1:] {
		out[d] = treeItem{dir: true}
	}
	for p, d := range s.content {
		out[p] = treeItem{sum: sum(d)}
	}
	return out
}

// mounted walks the mounted copy below the storm's top, reading every file.
func (s *storm) mounted() map[string]treeItem {
	out := map[string]treeItem{}
	var walk func(rel string)
	walk = func(rel string) {
		infos, _, err := s.e.fs.ReadDir(context.Background(), s.abs(rel), "", 0)
		s.must("list "+rel, err)
		for _, in := range infos {
			p := join(rel, in.Name)
			if in.IsDir {
				out[p] = treeItem{dir: true}
				walk(p)
				continue
			}
			data, err := s.e.readFile(s.abs(p))
			s.must("read "+p, err)
			out[p] = treeItem{sum: sum(data)}
		}
	}
	walk("")
	return out
}

func (s *storm) onDrive() map[string]treeItem {
	entries, ok := s.e.fd.Tree(stormTop)
	if !ok {
		s.t.Fatal("the storm's folder is not on the drive")
	}
	out := map[string]treeItem{}
	for _, en := range entries {
		out[en.Path] = treeItem{dir: en.Kind == "folder", sum: en.SHA256}
	}
	return out
}

func diffTrees(a, b map[string]treeItem, aName, bName string) []string {
	var out []string
	for p, x := range a {
		y, ok := b[p]
		switch {
		case !ok:
			out = append(out, "only "+aName+": "+p)
		case x.dir != y.dir:
			out = append(out, p+": a folder in one, a file in the other")
		case !x.dir && x.sum != y.sum:
			out = append(out, p+": content differs")
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, "only "+bName+": "+p)
		}
	}
	sort.Strings(out)
	return out
}

func (s *storm) compare(when string) {
	s.t.Helper()
	if os.Getenv("OLSYN_STORM_HISTORY") != "" {
		defer func() {
			if s.t.Failed() {
				var all []string
				entries, _ := s.e.fd.Tree(stormTop)
				for _, en := range entries {
					all = append(all, en.Path+" "+en.ID)
				}
				s.t.Logf("drive:\n%s", strings.Join(all, "\n"))
			}
		}()
	}
	want, local, drive := s.want(), s.mounted(), s.onDrive()
	report := func(what string, d []string) {
		if len(d) == 0 {
			return
		}
		if len(d) > 12 {
			d = append(d[:12], fmt.Sprintf("… and %d more", len(d)-12))
		}
		s.t.Errorf("%s, %s: %d differences:\n  %s", when, what, len(d), strings.Join(d, "\n  "))
		if os.Getenv("OLSYN_STORM_HISTORY") != "" {
			entries, _ := s.e.fd.Tree(stormTop)
			for _, en := range entries {
				for _, line := range d {
					if strings.HasSuffix(line, en.Path) {
						s.t.Logf("  on the drive: %s id %s, %d versions", en.Path, en.ID, en.Versions)
					}
				}
			}
			for _, line := range d {
				name := path.Base(line)
				for _, h := range s.history {
					if strings.Contains(h, name+" ") || strings.HasSuffix(h, name) {
						s.t.Logf("  %s", h)
					}
				}
			}
		}
	}
	report("the mounted copy against what was done", diffTrees(local, want, "on the mounted copy", "in the model"))
	report("the drive against the mounted copy", diffTrees(drive, local, "on the drive", "on the mounted copy"))
}
