package main

import (
	"bytes"
	"strings"
	"testing"

	"olsyn-drive/internal/auth"
	"olsyn-drive/internal/fakedrive"
)

func TestFolderID(t *testing.T) {
	id := "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	for _, s := range []string{id, "https://drive.example.com/n/" + id, "https://drive.example.com/folders/" + strings.ToUpper(id) + "?view=grid"} {
		if got, err := folderID(s); err != nil || got != id {
			t.Errorf("%q: %q %v", s, got, err)
		}
	}
}

func TestInstallPlans(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, un := range []bool{false, true} {
			plan := installPlan(goos, "/opt/olsyn drive/olsyn-drive", "/home/alex", un)
			if len(plan) == 0 {
				t.Fatalf("%s: empty plan", goos)
			}
			for _, s := range plan {
				for _, a := range s.cmd {
					if strings.Contains(a, "odt_") {
						t.Fatal("a token in a command line")
					}
				}
			}
		}
	}
	w := installPlan("windows", `C:\Users\alex\Downloads\olsyn-drive.exe`, `C:\Users\alex`, false)
	if !strings.Contains(strings.Join(w[1].cmd, " "), `CurrentVersion\Run`) || !strings.Contains(w[1].cmd[8], `\Programs\Olsyn Drive\olsyn-drive.exe" relay --background`) {
		t.Fatalf("windows: %v", w[1].cmd)
	}
	if !strings.Contains(launchAgent("/a&b", "/l"), "/a&amp;b") {
		t.Fatal("plist not escaped")
	}
}

func TestLoginWithToken(t *testing.T) {
	d := fakedrive.New()
	defer d.Close()
	home := t.TempDir()
	t.Setenv("OLSYN_DRIVE_HOME", home)
	var out bytes.Buffer
	if err := loginWithToken(t.Context(), d.URL, home, strings.NewReader("odt_wrong\n"), &out); err == nil {
		t.Fatal("accepted a token the drive refuses")
	}
	if err := loginWithToken(t.Context(), d.URL, home, strings.NewReader("odt_EXAMPLE\n"), &out); err != nil {
		t.Fatal(err)
	}
	l, tok, err := (auth.Store{Dir: home}).Load()
	if err != nil || tok != "odt_EXAMPLE" || l.NodeName != "Test node" {
		t.Fatalf("%v %q %+v", err, tok, l)
	}
}
