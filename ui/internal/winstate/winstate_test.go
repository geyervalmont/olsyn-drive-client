package winstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "window.json")
	if _, ok, err := Load(path); ok || err != nil {
		t.Fatalf("nothing saved: ok=%v err=%v", ok, err)
	}
	want := State{Bounds: Rect{X: 10, Y: 20, Width: 1000, Height: 700}, Maximised: true}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(path)
	if err != nil || !ok || got != want {
		t.Fatalf("Load = %+v %v %v", got, ok, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Load(path); ok || err == nil {
		t.Fatal("a broken file was accepted")
	}
}

func TestPlace(t *testing.T) {
	primary := Rect{X: 0, Y: 0, Width: 1920, Height: 1040}
	left := Rect{X: -1280, Y: 0, Width: 1280, Height: 984}
	screens := []Rect{primary, left}
	const minW, minH = 760, 520

	cases := []struct {
		name string
		in   Rect
		want Rect
		ok   bool
	}{
		{"fits", Rect{100, 80, 980, 680}, Rect{100, 80, 980, 680}, true},
		{"on the second screen", Rect{-1200, 50, 980, 680}, Rect{-1200, 50, 980, 680}, true},
		{"hangs off the right edge", Rect{1500, 100, 980, 680}, Rect{940, 100, 980, 680}, true},
		{"too big for the screen", Rect{0, 0, 4000, 3000}, Rect{0, 0, 1920, 1040}, true},
		{"smaller than the minimum", Rect{50, 50, 300, 200}, Rect{50, 50, 760, 520}, true},
		{"screen unplugged", Rect{3000, 100, 980, 680}, Rect{}, false},
		{"title bar above every screen", Rect{100, -600, 980, 680}, Rect{}, false},
		{"nothing saved", Rect{}, Rect{}, false},
	}
	for _, c := range cases {
		got, ok := Place(State{Bounds: c.in}, screens, minW, minH)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: Place(%+v) = %+v %v, want %+v %v", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}
