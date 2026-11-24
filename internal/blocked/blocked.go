// Package blocked recognises when security software has removed or blocked
// this program, so the app and the CLI can say so in plain words instead of
// "isn't answering", and point to what the person can do. It only looks: it
// never changes security settings, adds exclusions or works around a block.
//
// It keeps two small facts in the data folder's state directory (which
// security software leaves alone when it removes the program): where the
// program was installed, and when the desktop app last started the agent.
package blocked

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// HelpURL is the page that explains what to do (docs/pre-release-windows.md
// in the public repository).
const HelpURL = "https://github.com/geyervalmont/olsyn-drive-client/blob/main/docs/pre-release-windows.md"

// Kind is what points to security software.
type Kind string

const (
	// Removed: the program file is gone (quarantined or deleted).
	Removed Kind = "removed"
	// Refused: Windows refused to start it (a virus or policy block).
	Refused Kind = "refused"
	// Stopped: it was started again and again in a short time and still
	// does not answer.
	Stopped Kind = "stopped"
)

// Error is an agent that cannot run because of security software (most
// likely). Error() is the full sentence for a person; Title, Detail and
// HelpURL are its parts for a screen.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string {
	return e.Title() + " " + e.Detail() + " See " + HelpURL
}

func (e *Error) Unwrap() error { return e.Err }

// Title says what happened.
func (e *Error) Title() string {
	who := "Windows Security"
	where := "this PC"
	if runtime.GOOS != "windows" {
		who, where = "Security software", "this computer"
	}
	if e.Kind == Stopped {
		return "Olsyn Drive keeps being stopped on " + where + ", most likely by " + who + "."
	}
	return who + " removed or blocked Olsyn Drive on " + where + "."
}

// Detail says what is safe.
func (e *Error) Detail() string {
	return "Your files waiting to upload are safe."
}

// Repeated starts within StopWindow that still leave the agent silent are
// Stopped.
const (
	StopWindow = 10 * time.Minute
	StopCount  = 3
	keepStarts = 10
)

// Record reads and writes the facts in Dir (the data folder's state
// directory). Now defaults to time.Now.
type Record struct {
	Dir string
	Now func() time.Time
}

func (r Record) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

type installNote struct {
	Exe string    `json:"exe"`
	At  time.Time `json:"at"`
}

func (r Record) installPath() string { return filepath.Join(r.Dir, "installed.json") }
func (r Record) startsPath() string  { return filepath.Join(r.Dir, "agent-starts.json") }

// NoteInstalled remembers where the program was installed (after the person
// agreed to install it).
func (r Record) NoteInstalled(exe string) error {
	return r.write(r.installPath(), installNote{Exe: exe, At: r.now().UTC()})
}

// ForgetInstalled is for an uninstall: the program is gone on purpose.
func (r Record) ForgetInstalled() error {
	if err := os.Remove(r.installPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("forget the install: %w", err)
	}
	return nil
}

// InstalledExe is the program the last install put in place, if any.
func (r Record) InstalledExe() (string, bool) {
	var n installNote
	if !r.read(r.installPath(), &n) || n.Exe == "" {
		return "", false
	}
	return n.Exe, true
}

// InstalledProgramMissing reports an install that was recorded and whose
// program file is no longer there.
func (r Record) InstalledProgramMissing() (string, bool) {
	exe, ok := r.InstalledExe()
	if !ok {
		return "", false
	}
	_, err := os.Stat(exe)
	return exe, errors.Is(err, fs.ErrNotExist)
}

// NoteStart records that the agent is being started now.
func (r Record) NoteStart() error {
	var starts []time.Time
	r.read(r.startsPath(), &starts)
	starts = append(starts, r.now().UTC())
	if len(starts) > keepStarts {
		starts = starts[len(starts)-keepStarts:]
	}
	return r.write(r.startsPath(), starts)
}

// RecentStarts counts the starts within window of now.
func (r Record) RecentStarts(window time.Duration) int {
	var starts []time.Time
	r.read(r.startsPath(), &starts)
	cut := r.now().Add(-window)
	n := 0
	for _, t := range starts {
		if t.After(cut) {
			n++
		}
	}
	return n
}

// Diagnose looks at why the agent is not running: startErr is the error from
// starting it (nil when it was not started here). It returns "" when nothing
// points to security software.
func (r Record) Diagnose(startErr error) Kind {
	if startErr != nil {
		if errors.Is(startErr, fs.ErrNotExist) {
			return Removed
		}
		if refusedBySecurity(startErr) {
			return Refused
		}
	}
	if _, missing := r.InstalledProgramMissing(); missing {
		return Removed
	}
	if r.RecentStarts(StopWindow) >= StopCount {
		return Stopped
	}
	return ""
}

// Explain wraps err in an *Error when Diagnose finds security software
// behind it, and returns err otherwise.
func (r Record) Explain(err error) error {
	if k := r.Diagnose(err); k != "" {
		return &Error{Kind: k, Err: err}
	}
	return err
}

func (r Record) read(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func (r Record) write(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", r.Dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("save %s: %w", filepath.Base(path), err)
	}
	return nil
}
