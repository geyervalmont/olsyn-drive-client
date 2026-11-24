// Package winstate remembers where the main window was: its bounds and
// whether it was maximised, in a small JSON file, and decides whether those
// bounds still fit the screens when the app starts again.
package winstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Rect is a rectangle in device-independent pixels.
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// State is what is remembered about a window.
type State struct {
	Bounds    Rect `json:"bounds"`
	Maximised bool `json:"maximised"`
}

// Load reads a saved state. ok is false when nothing was saved or the file
// is unreadable (then the window opens at its default place).
func Load(path string) (s State, ok bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read window state: %w", err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, false, fmt.Errorf("parse window state: %w", err)
	}
	return s, true, nil
}

// Save writes the state through a temporary file.
func Save(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode window state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save window state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save window state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("save window state: %w", err)
	}
	return nil
}

// grip is how much of the top of a window must be on a screen for it to be
// reachable with the mouse: a strip of its title bar.
const (
	gripWidth  = 120
	gripHeight = 32
)

// Place returns where a window saved as s goes on these work areas. ok is
// false when it should open at its default place instead: no saved size, or
// its title bar is on no screen now (a monitor was unplugged, say). The size
// is kept within [minW, minH] and the work area it is on.
func Place(s State, workAreas []Rect, minW, minH int) (Rect, bool) {
	b := s.Bounds
	if b.Width <= 0 || b.Height <= 0 {
		return Rect{}, false
	}
	for _, wa := range workAreas {
		if wa.Width <= 0 || wa.Height <= 0 {
			continue
		}
		// The title strip: the top of the window, as wide as the window up
		// to gripWidth, measured from its left edge.
		grip := Rect{X: b.X, Y: b.Y, Width: min(b.Width, gripWidth), Height: gripHeight}
		if !overlaps(grip, wa, min(grip.Width, gripWidth)/2, gripHeight/2) {
			continue
		}
		b.Width = clamp(b.Width, minW, wa.Width)
		b.Height = clamp(b.Height, minH, wa.Height)
		// Pull it fully back onto this work area where it hangs over an edge.
		b.X = clamp(b.X, wa.X, wa.X+wa.Width-b.Width)
		b.Y = clamp(b.Y, wa.Y, wa.Y+wa.Height-b.Height)
		return b, true
	}
	return Rect{}, false
}

// overlaps reports whether a and b share at least w by h pixels.
func overlaps(a, b Rect, w, h int) bool {
	dx := min(a.X+a.Width, b.X+b.Width) - max(a.X, b.X)
	dy := min(a.Y+a.Height, b.Y+b.Height) - max(a.Y, b.Y)
	return dx >= w && dy >= h
}

// clamp keeps v within [lo, hi]; when hi < lo, lo wins.
func clamp(v, lo, hi int) int {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}
