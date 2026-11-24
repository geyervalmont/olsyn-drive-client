package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUILaunchArguments(t *testing.T) {
	for args, want := range map[string]bool{
		"":                              true,
		"ui":                            true,
		"ui --background":               true,
		"--background":                  true,
		"olsyn-drive://show/transfers":  true,
		"status":                        false,
		"send a --to /b":                false,
		"agent":                         false,
		"olsyn-drive-not-a-link status": false,
	} {
		if got := isUILaunch(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
	if hasUI {
		t.Skip("built with the desktop app")
	}
	t.Setenv("OLSYN_DRIVE_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"ui"}, nil, &out, &errb); code != exitUsage || !strings.Contains(errb.String(), "no desktop app") {
		t.Fatalf("ui verb without the app: %d %q", code, errb.String())
	}
}
