package transfer_test

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/localfs"
	"github.com/geyervalmont/olsyn-drive-client/internal/transfer"
)

type outcomes struct {
	mu    sync.Mutex
	by    map[string]string // drive path → outcome
	codes map[string]string
	found int
}

func (o *outcomes) report() transfer.SendReport {
	o.by, o.codes = map[string]string{}, map[string]string{}
	return transfer.SendReport{
		Found: func(transfer.SendFile) { o.mu.Lock(); o.found++; o.mu.Unlock() },
		Result: func(f transfer.SendFile, outcome, code, reason string) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.by[f.DrivePath], o.codes[f.DrivePath] = outcome, code
		},
	}
}

func (o *outcomes) count(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, v := range o.by {
		if v == outcome {
			n++
		}
	}
	return n
}

// treeSum fingerprints a folder (names, sizes, times, content) to prove a
// send left it untouched.
func treeSum(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	var paths []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		paths = append(paths, p)
		return err
	})
	sort.Strings(paths)
	for _, p := range paths {
		st, _ := os.Lstat(p)
		h.Write([]byte(p + st.ModTime().String() + st.Mode().String()))
		if st.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			h.Write(b)
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestSendFolderRerunConflictsAndOnlineOnly(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	s := &transfer.Sender{API: api, Engine: eng, Backoff: fastBackoff}
	src := filepath.Join(t.TempDir(), "Tower")
	files := map[string][]byte{
		"Plans/level-3.pdf":  payload(300<<10, 1), // multipart
		"Plans/level-4.pdf":  payload(10, 2),
		"Models/tower.ifc":   payload(70<<10, 3),
		"Models/.DS_Store":   []byte("junk"),
		"Models/~$notes.doc": []byte("temp"),
		"Cloud/online.dwg":   payload(5, 4),
		"readme.txt":         []byte("hello"),
	}
	for rel, b := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	single := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(single, []byte("single file"), 0o644)
	before := treeSum(t, src)
	dest := fd.AddFolder(fd.RootID(), "Projects")
	// a different file already has one of the names: hold, leave it alone
	towerOnDrive := fd.AddFolder(dest, "Tower")
	fd.AddFile(towerOnDrive, "readme.txt", []byte("someone else's"))
	placeholder := func(p string, info fs.FileInfo) localfs.Attrs {
		a := localfs.AttrsOf(p, info)
		a.Placeholder = filepath.Base(p) == "online.dwg"
		return a
	}
	spec := transfer.SendSpec{BatchID: "b1", Sources: []string{src, single}, ParentID: dest, ParentPath: "/Projects", Attrs: placeholder}
	var o outcomes
	if err := s.Send(ctxT(t), spec, o.report()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/Projects/Tower/Plans/level-3.pdf":  transfer.SendDone,
		"/Projects/Tower/Plans/level-4.pdf":  transfer.SendDone,
		"/Projects/Tower/Models/tower.ifc":   transfer.SendDone,
		"/Projects/Tower/Models/~$notes.doc": transfer.SendSkipped,
		"/Projects/Tower/Cloud/online.dwg":   transfer.SendNotLocal,
		"/Projects/Tower/readme.txt":         transfer.SendHeld,
		"/Projects/notes.txt":                transfer.SendDone,
	}
	for p, w := range want {
		if o.by[p] != w {
			t.Errorf("%s: %q, want %q", p, o.by[p], w)
		}
	}
	if len(o.by) != len(want) {
		t.Errorf("outcomes %v", o.by)
	}
	if o.codes["/Projects/Tower/readme.txt"] != driveapi.CodeNameConflict {
		t.Errorf("conflict code %q", o.codes["/Projects/Tower/readme.txt"])
	}
	if fi, ok := fd.Lookup("/Projects/Tower/Plans/level-3.pdf"); !ok || !bytes.Equal(fi.Data, files["Plans/level-3.pdf"]) {
		t.Fatal("content differs on the drive")
	}
	if fi, _ := fd.Lookup("/Projects/Tower/readme.txt"); string(fi.Data) != "someone else's" {
		t.Fatal("the drive's file was overwritten")
	}
	if _, ok := fd.Lookup("/Projects/Tower/Models/.DS_Store"); ok {
		t.Fatal("junk was sent")
	}
	if treeSum(t, src) != before {
		t.Fatal("the source changed")
	}

	// again: everything already there is skipped, nothing is sent twice
	uploads := fd.Count("start_upload")
	var o2 outcomes
	if err := s.Send(ctxT(t), spec, o2.report()); err != nil {
		t.Fatal(err)
	}
	if o2.count(transfer.SendSkipped) != 5 || o2.count(transfer.SendDone) != 0 || fd.Count("start_upload") != uploads {
		t.Fatalf("rerun: %v (uploads %d → %d)", o2.by, uploads, fd.Count("start_upload"))
	}

	// keep both: the clash is sent under a new name
	spec.Conflict = transfer.OnConflictKeepBoth
	spec.Sources = []string{src}
	var o3 outcomes
	if err := s.Send(ctxT(t), spec, o3.report()); err != nil {
		t.Fatal(err)
	}
	if o3.by["/Projects/Tower/readme.txt"] != transfer.SendDone || len(fd.List(towerOnDrive)) != 4 {
		t.Fatalf("keep both: %v, drive has %v", o3.by, fd.List(towerOnDrive))
	}
}

func TestSendWaitsOutAnOfflineDriveAndHoldsRefusals(t *testing.T) {
	fd, api := setup(t)
	eng := transfer.NewSimple(api, transfer.Options{Backoff: fastBackoff})
	s := &transfer.Sender{API: api, Engine: eng, Backoff: fastBackoff}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.bin")
	os.WriteFile(a, payload(1000, 5), 0o644)
	sealed := fd.AddFolder(fd.RootID(), "Issued")
	fd.RefuseWrites(sealed, driveapi.CodeSealed)
	var o outcomes
	rep := o.report()
	var waits sync.WaitGroup
	waits.Add(1)
	var once sync.Once
	rep.Waiting = func(transfer.SendFile, error, time.Time) { once.Do(waits.Done) }
	fd.SetOffline(true)
	go func() { waits.Wait(); fd.SetOffline(false) }()
	if err := s.Send(ctxT(t), transfer.SendSpec{BatchID: "b2", Sources: []string{a}, ParentID: fd.RootID(), ParentPath: "/"}, rep); err != nil {
		t.Fatal(err)
	}
	if o.by["/a.bin"] != transfer.SendDone {
		t.Fatalf("offline then online: %v", o.by)
	}
	var o2 outcomes
	if err := s.Send(ctxT(t), transfer.SendSpec{BatchID: "b3", Sources: []string{a}, ParentID: sealed, ParentPath: "/Issued"}, o2.report()); err != nil {
		t.Fatal(err)
	}
	if o2.by["/Issued/a.bin"] != transfer.SendHeld || o2.codes["/Issued/a.bin"] != driveapi.CodeSealed {
		t.Fatalf("sealed: %v %v", o2.by, o2.codes)
	}
}
