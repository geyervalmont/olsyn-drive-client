package platform

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// RemoveAfterExit deletes dir a few seconds after this process exits (the
// running executable inside it cannot be deleted while it runs).
func RemoveAfterExit(dir string) error {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\"%&|^<>!") {
		return fmt.Errorf("refusing to schedule removal of %q", dir)
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("find system folder: %w", err)
	}
	cmdExe := filepath.Join(sys, "cmd.exe")
	cmd := exec.Command(cmdExe)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `"` + cmdExe + `" /d /c ping -n 4 127.0.0.1 >nul & rmdir /s /q "` + dir + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("schedule removal: %w", err)
	}
	return cmd.Process.Release()
}
