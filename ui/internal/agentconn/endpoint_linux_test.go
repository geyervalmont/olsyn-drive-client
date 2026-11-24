package agentconn

import (
	"errors"
	"testing"
)

func TestLinuxSocketPath(t *testing.T) {
	if got := linuxSocketPath("/run/user/1000", nil); got != "/run/user/1000/olsyn-drive/agent.sock" {
		t.Fatal(got)
	}
	if got := linuxSocketPath("", func() (string, error) { return "/home/x", nil }); got != "/home/x/.local/state/olsyn-drive/run/agent.sock" {
		t.Fatal(got)
	}
	if got := linuxSocketPath("", func() (string, error) { return "", errors.New("no home") }); got == "" {
		t.Fatal("empty path")
	}
}
