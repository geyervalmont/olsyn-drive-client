package platform

// What holds a drive letter (DriveLettersInUse).
const (
	LetterVolume  = "volume"  // a disk, card, USB stick or optical drive
	LetterNetwork = "network" // a mapped network drive, connected or only remembered
	LetterSubst   = "subst"   // a folder made into a drive with subst
)

// LetterProbe is what Windows says about drive letters; ClassifyLetters
// turns it into a map. It is separate so the rules are tested on any OS.
type LetterProbe struct {
	// Mask is GetLogicalDrives: bit 0 is A:.
	Mask uint32
	// Remote reports a letter in Mask that is a network drive.
	Remote func(letter string) bool
	// Subst reports a letter in Mask that is a subst'd folder.
	Subst func(letter string) bool
	// Remembered reports a network mapping Windows keeps for a letter that
	// is not connected now (net use shows it as "Unavailable", Explorer
	// lists it under This PC): WNetGetConnection or HKCU\Network\<letter>.
	Remembered func(letter string) bool
}

// ClassifyLetters lists the letters D: to Z: that something holds.
func ClassifyLetters(p LetterProbe) map[string]string {
	used := map[string]string{}
	for i := 3; i < 26; i++ {
		l := string(rune('A'+i)) + ":"
		switch {
		case p.Mask&(1<<uint(i)) != 0:
			switch {
			case p.Subst != nil && p.Subst(l):
				used[l] = LetterSubst
			case p.Remote != nil && p.Remote(l):
				used[l] = LetterNetwork
			default:
				used[l] = LetterVolume
			}
		case p.Remembered != nil && p.Remembered(l):
			used[l] = LetterNetwork
		}
	}
	return used
}

// FreeLetters lists D: to Z: minus inUse, last first (W:-style letters near
// the end are the usual choice for network drives).
func FreeLetters(inUse map[string]string) []string {
	free := []string{}
	for i := 25; i >= 3; i-- {
		l := string(rune('A'+i)) + ":"
		if inUse[l] == "" {
			free = append(free, l)
		}
	}
	return free
}

// SuggestLetter picks want when it is free, otherwise the next free letter
// after it (towards Z:), then the nearest before it. "" when none is free.
func SuggestLetter(want string, inUse map[string]string) string {
	if len(want) != 2 || want[0] < 'D' || want[0] > 'Z' {
		want = "W:"
	}
	free := func(c byte) bool { return inUse[string(rune(c))+":"] == "" }
	if free(want[0]) {
		return want
	}
	for c := want[0] + 1; c <= 'Z'; c++ {
		if free(c) {
			return string(rune(c)) + ":"
		}
	}
	for c := want[0] - 1; c >= 'D'; c-- {
		if free(c) {
			return string(rune(c)) + ":"
		}
	}
	return ""
}
