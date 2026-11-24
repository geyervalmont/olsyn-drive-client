//go:build !windows

package main

// attachConsole is only needed for the Windows GUI-subsystem build.
func attachConsole() {}

// showError has nothing to show on other systems: the error is printed.
func showError(title, text string) {}
