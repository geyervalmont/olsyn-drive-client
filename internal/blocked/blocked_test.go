package blocked

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagnose(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	r := Record{Dir: filepath.Join(dir, "state"), Now: func() time.Time { return now }}

	if k := r.Diagnose(nil); k != "" {
		t.Fatalf("nothing recorded: %q", k)
	}
	// Starting a program file that is gone.
	startErr := fmt.Errorf("start agent: %w", &fs.PathError{Op: "stat", Path: "x", Err: fs.ErrNotExist})
	if k := r.Diagnose(startErr); k != Removed {
		t.Fatalf("missing exe at start: %q", k)
	}
	// Any other start failure is not blamed on security software.
	if k := r.Diagnose(errors.New("out of memory")); k != "" {
		t.Fatalf("other start error: %q", k)
	}

	// An install that is still there, then removed.
	exe := filepath.Join(dir, "Programs", "olsyn-drive")
	os.MkdirAll(filepath.Dir(exe), 0o700)
	os.WriteFile(exe, []byte("program"), 0o700)
	if err := r.NoteInstalled(exe); err != nil {
		t.Fatal(err)
	}
	if k := r.Diagnose(nil); k != "" {
		t.Fatalf("installed program present: %q", k)
	}
	os.Remove(exe)
	if k := r.Diagnose(nil); k != Removed {
		t.Fatalf("installed program removed: %q", k)
	}
	if err := r.ForgetInstalled(); err != nil {
		t.Fatal(err)
	}
	if k := r.Diagnose(nil); k != "" {
		t.Fatalf("after uninstall: %q", k)
	}

	// Started again and again within the window: stopped.
	for i := range StopCount {
		now = now.Add(time.Duration(i) * time.Minute)
		if err := r.NoteStart(); err != nil {
			t.Fatal(err)
		}
	}
	if k := r.Diagnose(nil); k != Stopped {
		t.Fatalf("repeated starts: %q", k)
	}
	now = now.Add(StopWindow)
	if k := r.Diagnose(nil); k != "" {
		t.Fatalf("starts long ago: %q", k)
	}
	// Only the last few starts are kept.
	for range 30 {
		r.NoteStart()
	}
	if n := r.RecentStarts(time.Hour); n != keepStarts {
		t.Fatalf("kept %d starts", n)
	}
}

func TestErrorWords(t *testing.T) {
	r := Record{Dir: t.TempDir()}
	err := r.Explain(&fs.PathError{Op: "stat", Path: "x", Err: fs.ErrNotExist})
	var be *Error
	if !errors.As(err, &be) || be.Kind != Removed {
		t.Fatalf("explain: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"removed or blocked Olsyn Drive", "Your files waiting to upload are safe.", HelpURL} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Error("the cause is lost")
	}
	if plain := errors.New("x"); r.Explain(plain) != plain {
		t.Error("an unrelated error was wrapped")
	}
	if s := (&Error{Kind: Stopped}).Title(); !strings.Contains(s, "keeps being stopped") {
		t.Errorf("stopped title %q", s)
	}
}
