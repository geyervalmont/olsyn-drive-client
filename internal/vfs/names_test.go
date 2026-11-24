package vfs

import (
	"testing"
	"time"
)

func TestNameMappingIsReversible(t *testing.T) {
	cases := map[string]string{
		"plain.txt":     "plain.txt",
		"a<b>c:d.txt":   "a＜b＞c：d.txt",
		"what?.txt":     "what？.txt",
		"trailing.":     "trailing．",
		"space ":        "space␠",
		"CON":           "COＮ",
		"nul.txt":       "nuｌ.txt",
		"com1.log":      "coｍ1.log",
		"real＜char.txt": "real‛＜char.txt",
		"quote‛.txt":    "quote‛‛.txt",
		"full ｗidth":    "full ‛ｗidth",
		"Console.txt":   "Console.txt",
	}
	for drive, local := range cases {
		if got := EncodeName(drive); got != local {
			t.Errorf("EncodeName(%q) = %q, want %q", drive, got, local)
		}
		if back := DecodeName(EncodeName(drive)); back != drive {
			t.Errorf("round trip %q -> %q", drive, back)
		}
	}
}

func TestJunkAndConflictNames(t *testing.T) {
	for _, n := range []string{"Thumbs.db", "desktop.ini", ".DS_Store", "~$report.docx", "._photo.jpg", "~WRL0001.tmp", ".~lock.a.odt#"} {
		if !IsJunk(n) {
			t.Errorf("%q should be junk", n)
		}
	}
	for _, n := range []string{"report.docx", "temp.tmp", "db.thumbs"} {
		if IsJunk(n) {
			t.Errorf("%q is not junk", n)
		}
	}
	at := time.Date(2026, 11, 24, 15, 30, 0, 0, time.Local)
	if got := ConflictName("Plan.pdf", "PC-1", at); got != "Plan (conflict from PC-1, 2026-11-24 1530).pdf" {
		t.Errorf("conflict name %q", got)
	}
}
