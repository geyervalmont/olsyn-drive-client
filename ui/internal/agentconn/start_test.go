//go:build !windows

package agentconn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The app starts "olsyn-drive agent" with no flags; a dev run points both at
// a test home and a fake drive through OLSYN_DRIVE_HOME and
// OLSYN_DRIVE_SERVER, so the child must see this process's environment.
func TestStarterPassesTheEnvironment(t *testing.T) {
	if os.Getenv("ODC_STARTER_CHILD") == "1" {
		out := os.Getenv("ODC_STARTER_OUT")
		data := os.Getenv("OLSYN_DRIVE_HOME") + "\n" + os.Getenv("OLSYN_DRIVE_SERVER")
		if err := os.WriteFile(out, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	out := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("ODC_STARTER_CHILD", "1")
	t.Setenv("ODC_STARTER_OUT", out)
	t.Setenv("OLSYN_DRIVE_HOME", "/tmp/odc-test-home")
	t.Setenv("OLSYN_DRIVE_SERVER", "http://127.0.0.1:8787")
	s := &Starter{Exe: os.Args[0], Args: []string{"-test.run=^TestStarterPassesTheEnvironment$"}}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil && strings.Contains(string(data), "8787") {
			if got := string(data); got != "/tmp/odc-test-home\nhttp://127.0.0.1:8787" {
				t.Fatalf("the child saw %q", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the started child never reported its environment")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
