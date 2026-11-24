package localfs

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	for _, n := range []string{".DS_Store", "Thumbs.db", "desktop.ini", "._plan.pdf", "SPOOL FULL - try again later.txt"} {
		if !IsJunk(n) {
			t.Errorf("%q should be junk", n)
		}
	}
	for _, n := range []string{"~$plan.docx", ".~lock.plan.odt#", "model.blend.tmp", "a.part", "b.crdownload", "c~", ".plan.pdf.sb-1234-abcd"} {
		if !IsTemp(n) {
			t.Errorf("%q should be temp", n)
		}
	}
	for _, n := range []string{"plan.pdf", "Level 3.rvt", "temple.obj", "parts.csv"} {
		if IsTemp(n) || IsJunk(n) {
			t.Errorf("%q should be sent", n)
		}
	}
	bad := map[string]bool{"": true, "..": true, "a/b": true, `a\b`: true, "x\x01": true, " lead": true, "trail.": true, string([]byte{0xff}): true}
	for n := range bad {
		if CheckName(n) == "" {
			t.Errorf("%q should be refused", n)
		}
	}
	if CheckName("Example Tower – Level 3 (final).ifc") != "" {
		t.Error("ordinary name refused")
	}
	for _, n := range []string{"CON", "con.txt", "LPT1.dwg", "a:b", "what?"} {
		if WindowsNameProblem(n) == "" {
			t.Errorf("%q should be a Windows problem", n)
		}
	}
	if WindowsNameProblem("console.txt") != "" {
		t.Error("console.txt is fine on Windows")
	}
}

func TestPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if _, err := Rel(root, filepath.Join(root, "..", "x")); err == nil {
		t.Fatal("Rel let a path leave root")
	}
	r, err := Rel(root, filepath.Join(root, "a", "b.txt"))
	if err != nil || r != "a/b.txt" {
		t.Fatalf("Rel: %q %v", r, err)
	}
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "a//b", "a/./b", `a\..\b`, ""} {
		if _, err := Join(root, bad); err == nil {
			t.Errorf("Join accepted %q", bad)
		}
	}
	if p, err := Join(root, "a/b.txt"); err != nil || p != filepath.Join(root, "a", "b.txt") {
		t.Fatalf("Join: %q %v", p, err)
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		must(os.WriteFile(p, []byte(body), 0o644))
	}
	write("Plans/level-3.pdf", "plan")
	write("Plans/.DS_Store", "x")
	write("Plans/~$notes.docx", "x")
	write("Models/tower.ifc", "ifc")
	write(".fseventsd/x", "x")
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644))
	if runtime.GOOS != "windows" {
		must(os.Symlink(outside, filepath.Join(root, "escape")))
		must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "Plans", "secret.txt")))
	}
	write("Plans/LEVEL-3.PDF", "plan")
	write("Cloud/online.dwg", "remote")
	var got []string
	var skips []Skip
	opt := WalkOptions{Attrs: func(p string, info fs.FileInfo) Attrs {
		a := AttrsOf(p, info)
		a.Placeholder = filepath.Base(p) == "online.dwg"
		return a
	}}
	err := Walk(context.Background(), root, opt, func(e Entry) error {
		got = append(got, e.Rel)
		return nil
	}, func(s Skip) { skips = append(skips, s) })
	must(err)
	slices.Sort(got)
	if !slices.Equal(got, []string{"Models/tower.ifc", "Plans/level-3.pdf"}) {
		t.Fatalf("walk: %v", got)
	}
	reasons := map[string]string{}
	for _, s := range skips {
		reasons[s.Rel] = s.Reason
	}
	if reasons["Plans/~$notes.docx"] == "" {
		t.Errorf("temp file not reported: %v", reasons)
	}
	if runtime.GOOS != "windows" && (reasons["escape"] == "" || reasons["Plans/secret.txt"] == "") {
		t.Errorf("links not reported: %v", reasons)
	}
	caseSensitive := false
	if des, _ := os.ReadDir(filepath.Join(root, "Plans")); len(des) == 5 { // level-3.pdf and LEVEL-3.PDF both there
		caseSensitive = true
	}
	if caseSensitive && (reasons["Plans/level-3.pdf"] != "" || !strings.Contains(reasons["Plans/LEVEL-3.PDF"], "upper/lower case")) {
		t.Errorf("case clash not reported: %v", reasons)
	}
	var notLocal int
	for _, s := range skips {
		if s.NotLocal {
			notLocal++
		}
	}
	if notLocal != 1 || reasons["Cloud/online.dwg"] == "" {
		t.Errorf("online-only file not reported: %v", reasons)
	}
	if _, ok := reasons["Plans/.DS_Store"]; ok {
		t.Error("junk should be skipped silently")
	}
}
