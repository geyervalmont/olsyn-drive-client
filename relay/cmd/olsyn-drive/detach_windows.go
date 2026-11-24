//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detach re-launches this command with no console window and returns
// true in the parent (which then exits). The child runs without
// --background, so it doesn't detach again.
func detach(args []string) bool {
	var rest []string
	for _, a := range args {
		if a != "--background" && a != "-background" {
			rest = append(rest, a)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	c := exec.Command(exe, rest...)
	const createNoWindow, detachedProcess = 0x08000000, 0x00000008
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | detachedProcess}
	return c.Start() == nil
}
