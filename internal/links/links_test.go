package links

import (
	"errors"
	"testing"
)

func TestScreen(t *testing.T) {
	good := map[string]string{
		"":                  DefaultScreen,
		"transfers":         "transfers/active",
		"Transfers/Held":    "transfers/held",
		"/transfers/done/":  "transfers/done",
		"transfers/skipped": "transfers/skipped",
		"activity":          "activity",
		"settings":          "settings",
		"account":           "account",
		"about":             "about",
		"popover":           "popover",
		"signin":            "signin",
	}
	for in, want := range good {
		got, err := Screen(in)
		if err != nil || got != want {
			t.Errorf("Screen(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"bogus", "transfers/nope", "settings/x", "about?x=1", "../settings", "transfers/held#x"} {
		if _, err := Screen(bad); !errors.Is(err, ErrUnknownScreen) {
			t.Errorf("Screen(%q) err = %v, want ErrUnknownScreen", bad, err)
		}
	}
}

func TestParse(t *testing.T) {
	cases := map[string]Action{
		"olsyn-drive://":                             {Kind: KindScreen, Screen: DefaultScreen},
		"olsyn-drive://open/settings":                {Kind: KindScreen, Screen: "settings"},
		"olsyn-drive://open/transfers/held":          {Kind: KindScreen, Screen: "transfers/held"},
		"olsyn-drive://open/popover":                 {Kind: KindScreen, Screen: "popover"},
		"OLSYN-DRIVE://Open/about":                   {Kind: KindScreen, Screen: "about"},
		"olsyn-drive://show/transfers/held":          {Kind: KindScreen, Screen: "transfers/held"},
		"olsyn-drive://open?path=/Projects/Tower":    {Kind: KindDrive, Path: "/Projects/Tower"},
		"olsyn-drive://drive/Projects/Tower":         {Kind: KindDrive, Path: "/Projects/Tower"},
		"olsyn-drive://web?path=/Projects":           {Kind: KindWeb, Path: "/Projects"},
		"olsyn-drive://web":                          {Kind: KindWeb, Path: ""},
		"olsyn-drive://open/Projects/Sample%20Tower": {Kind: KindDrive, Path: "/Projects/Sample Tower"},
	}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://example.com", "olsyn-drive://open/transfers/nope", "olsyn-drive://open/Projects/../../x", "olsyn-drive://show/../../etc", "olsyn-drive://drive/../x", "olsyn-drive://format/c"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestScreenLinkRoundTrip(t *testing.T) {
	for _, s := range []string{"transfers/held", "activity", "popover"} {
		a, err := Parse(ScreenLink(s))
		if err != nil || a.Kind != KindScreen || a.Screen != s {
			t.Errorf("round trip %q: %+v, %v", s, a, err)
		}
	}
}
