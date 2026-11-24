//go:build !desktopui

package main

// hasUI is false: this build is the agent and command line only (the
// desktop app is linked in with the desktopui tag; see the Makefile).
const hasUI = false

func runUI(e *env, args []string) int {
	e.errorf("olsyn-drive: this build has no desktop app (it is built with -tags desktopui); the commands below still work\n\n%s", usage)
	return exitUsage
}
