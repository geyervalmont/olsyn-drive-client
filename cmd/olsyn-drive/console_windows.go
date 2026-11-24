package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// attachConsole connects a GUI-subsystem build (the single exe with the
// desktop app) to the console of the shell that started it, so command
// output appears there. Handles that are already valid (a console build,
// or output redirected to a file or pipe) are left alone. Started without
// a console (at sign-in, from Explorer), it does nothing.
func attachConsole() {
	out, errOut := validStd(windows.STD_OUTPUT_HANDLE), validStd(windows.STD_ERROR_HANDLE)
	if out && errOut {
		return
	}
	const attachParentProcess = ^uintptr(0) // ATTACH_PARENT_PROCESS (DWORD -1)
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	if r, _, _ := proc.Call(attachParentProcess); r == 0 {
		return
	}
	if !out {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = f
			_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(f.Fd()))
		}
	}
	if !errOut {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stderr = f
			_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
		}
	}
	if !validStd(windows.STD_INPUT_HANDLE) {
		if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
			os.Stdin = f
			_ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(f.Fd()))
		}
	}
}

func validStd(which uint32) bool {
	h, err := windows.GetStdHandle(which)
	return err == nil && h != 0 && h != windows.InvalidHandle
}

// showError puts up a message box: the desktop app's way to say it could
// not start when there is no console to print to.
func showError(title, text string) {
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	_, _ = windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
