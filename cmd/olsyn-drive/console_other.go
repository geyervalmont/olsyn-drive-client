//go:build !windows

package main

// attachConsole is only needed for the Windows GUI-subsystem build.
func attachConsole() {}

// showError has nothing to show on other systems: the error is printed.
func showError(title, text string) {}

// showInfo has nothing to show on other systems.
func showInfo(title, text string) {}

// askOKCancel has no dialog on other systems: it answers Cancel.
func askOKCancel(title, text string) bool { return false }

// askYesNo has no dialog on other systems: it answers with the default.
func askYesNo(title, text string, def bool) bool { return def }
