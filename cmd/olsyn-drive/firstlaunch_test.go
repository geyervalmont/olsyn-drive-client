package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// firstLaunchSetup is a PC with the program in Downloads only: the data
// folder and the program folder do not exist.
func firstLaunchSetup(t *testing.T) (root string, e *env, d *launchDeps, asked *[]string) {
	t.Helper()
	root = t.TempDir()
	self := filepath.Join(root, "Downloads", platform.ExecutableName)
	if err := os.MkdirAll(filepath.Dir(self), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self, []byte("program"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	e = &env{ctx: context.Background(), paths: platform.Under(filepath.Join(root, "data")), stdout: &out, stderr: &errb}
	asked = &[]string{}
	d = &launchDeps{
		installDir:       func() (string, error) { return filepath.Join(root, "Programs", "Olsyn Drive"), nil },
		self:             func() (string, error) { return self, nil },
		installedVersion: func() (string, error) { return "", nil },
		ask: func(title, text string) bool {
			*asked = append(*asked, title+"\n"+text)
			return false
		},
		showError: func(title, text string) { t.Errorf("error dialog: %s: %s", title, text) },
		start:     func(string) error { t.Error("started a program"); return nil },
		install:   func() error { t.Error("installed without consent"); return nil },
	}
	return root, e, d, asked
}

// listFiles is every path under root, relative to it.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err == nil && p != root {
			rel, _ := filepath.Rel(root, p)
			got = append(got, rel)
		}
		return nil
	})
	return got
}

// Opened from Downloads and declined: the install question is the only
// thing that happens; nothing is written anywhere.
func TestFirstLaunchWritesNothingBeforeConsent(t *testing.T) {
	root, e, d, asked := firstLaunchSetup(t)
	before := listFiles(t, root)
	code, done := firstLaunch(e, *d)
	if !done || code != exitOK {
		t.Fatalf("firstLaunch = %d, %v", code, done)
	}
	if len(*asked) != 1 || !strings.HasPrefix((*asked)[0], "Install Olsyn Drive\n") {
		t.Fatalf("questions %q", *asked)
	}
	if after := listFiles(t, root); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("files written before the person agreed: before %v, after %v", before, after)
	}
}

// Agreeing installs (and only then).
func TestFirstLaunchInstallsAfterConsent(t *testing.T) {
	_, e, d, _ := firstLaunchSetup(t)
	installed := 0
	d.ask = func(string, string) bool { return true }
	d.install = func() error { installed++; return nil }
	if code, done := firstLaunch(e, *d); !done || code != exitOK || installed != 1 {
		t.Fatalf("firstLaunch = %d, %v; installs %d", code, done, installed)
	}
}

// The data folder is there but the program is not (removed by the person or
// by security software): the question says it reinstalls and keeps the
// waiting files.
func TestFirstLaunchReinstallKeepsWaitingFiles(t *testing.T) {
	root, e, d, asked := firstLaunchSetup(t)
	if err := os.MkdirAll(e.paths.State, 0o700); err != nil {
		t.Fatal(err)
	}
	before := listFiles(t, root)
	firstLaunch(e, *d)
	if len(*asked) != 1 {
		t.Fatalf("questions %q", *asked)
	}
	q := (*asked)[0]
	for _, want := range []string{"Reinstall Olsyn Drive", "keeps your waiting files", "pre-release-windows.md"} {
		if !strings.Contains(q, want) {
			t.Errorf("question lacks %q:\n%s", want, q)
		}
	}
	if after := listFiles(t, root); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("files written before the person agreed: %v", after)
	}
}

// Without an install folder there is nothing to offer: the program says so
// rather than running from Downloads (which would write logs there).
func TestFirstLaunchWithoutInstallDir(t *testing.T) {
	root, e, d, _ := firstLaunchSetup(t)
	shown := ""
	d.installDir = func() (string, error) { return "", fs.ErrNotExist }
	d.showError = func(title, text string) { shown = title }
	if _, done := firstLaunch(e, *d); !done || shown == "" {
		t.Fatalf("done %v, dialog %q", done, shown)
	}
	if after := listFiles(t, root); len(after) != 2 { // Downloads and the program in it
		t.Fatalf("files written: %v", after)
	}
}
