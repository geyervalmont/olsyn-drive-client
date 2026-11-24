package vfs_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/index"
)

// has reports whether the index holds the drive node sid.
func (e *env) has(sid string) bool {
	_, err := e.idx.BySID(sid)
	return err == nil
}

func (e *env) listedSID(sid string) bool {
	en, err := e.idx.BySID(sid)
	return err == nil && e.idx.Listed(en.ID)
}

func (e *env) names(path string) string {
	e.t.Helper()
	infos, _, err := e.fs.ReadDir(ctxT(e.t), path, "", 0)
	if err != nil {
		e.t.Fatalf("list %s: %v", path, err)
	}
	var out []string
	for _, i := range infos {
		out = append(out, i.Name)
	}
	return strings.Join(out, ",")
}

// A folder made on this PC that turns out to exist on the drive already
// (adopted) is listed when first opened, though the feed never mentioned
// what is in it.
func TestAdoptedFolderIsListedOnFirstOpen(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	e.names("/")
	var proj string
	e.fd.Quietly(func() {
		proj = e.fd.AddFolder(e.fd.RootID(), "Proj")
		e.fd.AddFile(proj, "plan.pdf", []byte("plan"))
	})
	e.mkdir("/Proj")
	waitFor(t, 10*time.Second, "the folder to be adopted", func() bool { return e.has(proj) })
	if e.listedSID(proj) {
		t.Fatal("an adopted folder was marked listed")
	}
	if got := e.names("/Proj"); got != "plan.pdf" {
		t.Fatalf("adopted folder shows %q", got)
	}
}

// A folder the feed upserts is still listed when first opened: the feed
// says the folder exists, not what is in it.
func TestFeedFolderIsListedOnFirstOpen(t *testing.T) {
	e := newEnv(t).start()
	defer e.stop()
	e.names("/")
	f := e.fd.AddFolder(e.fd.RootID(), "New")
	e.fd.Quietly(func() { e.fd.AddFile(f, "quiet.txt", []byte("q")) })
	waitFor(t, 5*time.Second, "the feed to bring the folder", func() bool { return e.has(f) })
	if got := e.names("/New"); got != "quiet.txt" {
		t.Fatalf("New shows %q", got)
	}
}

// A resync lists a visited folder again, also when the same page upserts
// that folder (in either order).
func TestResyncRelistsWhenThePageAlsoUpsertsTheFolder(t *testing.T) {
	for _, resyncFirst := range []bool{true, false} {
		e := newEnv(t)
		f := e.fd.AddFolder(e.fd.RootID(), "F")
		e.fd.AddFile(f, "a.txt", []byte("a"))
		e.start()
		if got := e.names("/F"); got != "a.txt" {
			t.Fatalf("F shows %q", got)
		}
		var b string
		e.fd.Quietly(func() { b = e.fd.AddFile(f, "b.txt", []byte("b")) })
		if resyncFirst {
			e.fd.EmitResync(f)
			e.fd.Rename(f, "F2")
		} else {
			e.fd.Rename(f, "F2")
			e.fd.EmitResync(f)
		}
		// listed again without being opened
		waitFor(t, 5*time.Second, "the resync to list F again", func() bool { return e.has(b) && e.listedSID(f) })
		if got := e.names("/F2"); got != "a.txt,b.txt" {
			t.Fatalf("resync first %v: F2 shows %q", resyncFirst, got)
		}
		e.stop()
	}
}

// A listing answered before a resync is not trusted: the folder is fetched
// again, so a file only the resync announced is not dropped as unseen.
func TestResyncDuringAListingFetchesAgain(t *testing.T) {
	e := newEnv(t)
	f := e.fd.AddFolder(e.fd.RootID(), "F")
	e.fd.AddFile(f, "a.txt", []byte("a"))
	e.start()
	defer e.stop()
	e.names("/")

	// hold the first listing of F with the answer it had before the change
	var once sync.Once
	captured, release := make(chan struct{}), make(chan struct{})
	path := "/api/v1/nodes/" + f + "/children"
	e.fd.SetIntercept(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != path {
			return false
		}
		first := false
		once.Do(func() { first = true })
		if !first {
			return false
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, e.fd.URL()+r.URL.RequestURI(), nil)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		close(captured)
		<-release
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return true
	})
	go func() { _, _, _ = e.fs.ReadDir(ctxT(t), "/F", "", 0) }()
	<-captured
	var b string
	e.fd.Quietly(func() { b = e.fd.AddFile(f, "b.txt", []byte("b")) })
	before := e.fd.Count("changes")
	e.fd.EmitResync(f)
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "the feed to be read", func() bool { return e.fd.Count("changes") >= before+2 })
	close(release)
	waitFor(t, 5*time.Second, "F to be listed again", func() bool { return e.has(b) })
	if got := e.names("/F"); got != "a.txt,b.txt" {
		t.Fatalf("F shows %q", got)
	}
	if n := e.fd.Count("children"); n < 3 { // root, F (stale), F again
		t.Fatalf("children calls %d", n)
	}
}

// After a reset the visited folders are listed again at once (the most
// recently listed first); others are listed when next opened, and what the
// index held stays until then.
func TestResetRelistsVisitedFoldersFirst(t *testing.T) {
	e := newEnv(t)
	a := e.fd.AddFolder(e.fd.RootID(), "A")
	bf := e.fd.AddFolder(e.fd.RootID(), "B")
	e.fd.AddFile(a, "a1.txt", []byte("1"))
	oldB := e.fd.AddFile(bf, "b1.txt", []byte("1"))
	e.start()
	defer e.stop()
	e.names("/A")
	waitFor(t, 5*time.Second, "B to be known", func() bool { return e.has(bf) })
	if e.listedSID(bf) {
		t.Fatal("B listed without being opened")
	}
	var a2, b2 string
	e.fd.Quietly(func() {
		a2 = e.fd.AddFile(a, "a2.txt", []byte("2"))
		b2 = e.fd.AddFile(bf, "b2.txt", []byte("2"))
	})
	e.fd.ForceReset("test")
	e.fs.Refresh()
	waitFor(t, 5*time.Second, "A to be listed again", func() bool { return e.has(a2) && e.listedSID(a) })
	if e.has(b2) || e.listedSID(bf) {
		t.Fatal("B (never opened) was listed by the reset")
	}
	if got := e.names("/B"); got != "b1.txt,b2.txt" {
		t.Fatalf("B shows %q", got)
	}
	_ = oldB
	var ls []index.Listing
	_ = e.idx.View(func(tx *index.Tx) error { ls, _ = tx.Listings(); return nil })
	if len(ls) < 3 {
		t.Fatalf("listings after reset: %+v", ls)
	}
}
