//go:build !windows

package main

// detach: launchd and systemd already run the client without a terminal.
func detach([]string) bool { return false }
