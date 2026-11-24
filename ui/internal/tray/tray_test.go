package tray

import (
	"bytes"
	"encoding/json"
	"image/png"
	"strings"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

func online() agentconn.State {
	letter := "W"
	return agentconn.State{Connection: "online", Account: &agentconn.Account{Name: "Sample Designer"}, Mount: agentconn.Mount{State: "mounted", Letter: &letter}}
}

func TestFromState(t *testing.T) {
	eta := int64(600)
	cases := []struct {
		name string
		edit func(*agentconn.State)
		kind Kind
		tip  string
	}{
		{"idle", func(*agentconn.State) {}, Idle, "Olsyn Drive (W:): all changes are uploaded"},
		{"signed out", func(s *agentconn.State) { s.Account = nil; s.Connection = "signed_out" }, Attention, "sign in"},
		{"held wins over syncing", func(s *agentconn.State) { s.Transfers = agentconn.Summary{Active: 3, Held: 2} }, Attention, "2 files could not be uploaded"},
		{"paused", func(s *agentconn.State) { s.Paused = true; s.Transfers.FilesLeft = 4 }, Paused, "paused with 4 files waiting"},
		{"offline", func(s *agentconn.State) { s.Connection = "offline"; s.Transfers.FilesLeft = 1 }, Offline, "1 file waiting"},
		{"syncing", func(s *agentconn.State) {
			s.Transfers = agentconn.Summary{Active: 2, FilesLeft: 5, BytesTotal: 200, BytesDone: 50, EtaSeconds: &eta}
		}, Syncing, "uploading, 5 files left, 25% done, about 10 minutes"},
		{"issue", func(s *agentconn.State) {
			s.Issues = []agentconn.Issue{{Code: "driver_missing", Message: "The drive needs a one-time install."}}
		}, Attention, "one-time install"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := online()
			c.edit(&s)
			v := FromState(s)
			if v.Kind != c.kind {
				t.Fatalf("kind = %s, want %s", v.Kind, c.kind)
			}
			if !strings.Contains(v.Tooltip, c.tip) {
				t.Fatalf("tooltip %q does not contain %q", v.Tooltip, c.tip)
			}
			if len([]rune(v.Tooltip)) > 127 {
				t.Fatalf("tooltip too long: %d", len([]rune(v.Tooltip)))
			}
		})
	}
}

func TestSyncingProgress(t *testing.T) {
	s := online()
	s.Transfers = agentconn.Summary{Active: 1, BytesTotal: 1000, BytesDone: 500}
	if got := FromState(s).Progress; got != 0.5 {
		t.Fatalf("progress = %v", got)
	}
	s.Transfers.BytesTotal = 0
	if got := FromState(s).Progress; got != -1 {
		t.Fatalf("unknown progress = %v", got)
	}
}

func TestTooltipIsShortened(t *testing.T) {
	s := online()
	s.Issues = []agentconn.Issue{{Message: strings.Repeat("word ", 60)}}
	tip := FromState(s).Tooltip
	if n := len([]rune(tip)); n > 127 || !strings.HasSuffix(tip, "…") {
		t.Fatalf("tooltip %d runes: %q", n, tip)
	}
}

func TestClickFor(t *testing.T) {
	withAction := func(open string) agentconn.Notification {
		n := agentconn.Notification{Kind: "held"}
		n.Action = &struct {
			Label string          `json:"label"`
			Open  json.RawMessage `json:"open"`
		}{Label: "Show", Open: json.RawMessage(open)}
		return n
	}
	if c := ClickFor(withAction(`{"target":"ui","path":"transfers/held"}`)); c.Route != "transfers/held" || c.Open != nil {
		t.Fatalf("ui action: %+v", c)
	}
	if c := ClickFor(withAction(`{"target":"drive","path":"/Projects/Sample"}`)); c.Open == nil || c.Open.Path != "/Projects/Sample" {
		t.Fatalf("drive action: %+v", c)
	}
	if c := ClickFor(agentconn.Notification{Kind: "update_failed"}); c.Route != "about" {
		t.Fatalf("update: %+v", c)
	}
	if c := ClickFor(agentconn.Notification{Kind: "something_new"}); c.Route != "activity" {
		t.Fatalf("unknown kind: %+v", c)
	}
}

func TestBucket(t *testing.T) {
	if Bucket(-1) != -1 || Bucket(0) != 0 || Bucket(0.5) != 6 || Bucket(1) != 12 {
		t.Fatal("bucket boundaries")
	}
}

func TestIconsDecode(t *testing.T) {
	for _, k := range []Kind{Idle, Syncing, Paused, Offline, Attention, Stopped} {
		for _, theme := range []Theme{LightTaskbar, DarkTaskbar} {
			b, err := Icon(View{Kind: k, Progress: 0.4}, theme, 32)
			if err != nil {
				t.Fatalf("%s: %v", k, err)
			}
			img, err := png.Decode(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("%s: decode: %v", k, err)
			}
			if img.Bounds().Dx() != 32 {
				t.Fatalf("%s: width %d", k, img.Bounds().Dx())
			}
			// The corner is transparent and the ring is drawn.
			if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
				t.Fatalf("%s: corner not transparent", k)
			}
			if _, _, _, a := img.At(16, 3).RGBA(); a == 0 {
				t.Fatalf("%s: ring missing at the top", k)
			}
		}
	}
	if _, err := Icon(View{Kind: Idle}, LightTaskbar, 8); err == nil {
		t.Fatal("tiny size accepted")
	}
}
