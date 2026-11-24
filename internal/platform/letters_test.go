package platform

import (
	"reflect"
	"testing"
)

func TestClassifyLetters(t *testing.T) {
	bit := func(l byte) uint32 { return 1 << uint(l-'A') }
	p := LetterProbe{
		Mask:       bit('C') | bit('D') | bit('P') | bit('S') | bit('W'),
		Remote:     func(l string) bool { return l == "P:" || l == "W:" },
		Subst:      func(l string) bool { return l == "S:" },
		Remembered: func(l string) bool { return l == "O:" || l == "W:" }, // O: mapped but disconnected
	}
	got := ClassifyLetters(p)
	want := map[string]string{"D:": LetterVolume, "O:": LetterNetwork, "P:": LetterNetwork, "S:": LetterSubst, "W:": LetterNetwork}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	free := FreeLetters(got)
	for _, l := range free {
		if got[l] != "" || l == "C:" {
			t.Fatalf("%s offered as free", l)
		}
	}
	if len(free) != 23-len(want) || free[0] != "Z:" {
		t.Fatalf("free %v", free)
	}
}

func TestSuggestLetter(t *testing.T) {
	cases := []struct {
		want  string
		inUse map[string]string
		out   string
	}{
		{"W:", nil, "W:"},
		{"W:", map[string]string{"W:": LetterNetwork}, "X:"},
		{"W:", map[string]string{"W:": "n", "X:": "n", "Y:": "n", "Z:": "n"}, "V:"},
		{"", map[string]string{}, "W:"},
		{"Z:", map[string]string{"Z:": "n"}, "Y:"},
	}
	for _, c := range cases {
		if got := SuggestLetter(c.want, c.inUse); got != c.out {
			t.Errorf("SuggestLetter(%q, %v) = %q, want %q", c.want, c.inUse, got, c.out)
		}
	}
	all := map[string]string{}
	for c := 'D'; c <= 'Z'; c++ {
		all[string(c)+":"] = LetterVolume
	}
	if got := SuggestLetter("W:", all); got != "" {
		t.Fatalf("all taken: %q", got)
	}
}
