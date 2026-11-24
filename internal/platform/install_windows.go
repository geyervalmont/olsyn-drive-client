package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// shortcutName is the Start menu entry's file name.
const shortcutName = ProductName + ".lnk"

// startMenuShortcut is the per-user Start menu entry's path.
func startMenuShortcut() (string, error) {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Programs, 0)
	if err != nil {
		return "", fmt.Errorf("find start menu folder: %w", err)
	}
	return filepath.Join(dir, shortcutName), nil
}

// CreateStartMenuShortcut adds "Olsyn Drive" to the person's Start menu,
// opening target with no arguments (the desktop app). It uses the Windows
// Script Host shell object, the documented way to write a .lnk file.
func CreateStartMenuShortcut(target string) error {
	path, err := startMenuShortcut()
	if err != nil {
		return err
	}
	// COM calls must stay on one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		var oe *ole.OleError
		if !errors.As(err, &oe) || oe.Code() != 1 { // S_FALSE: already initialised on this thread
			return fmt.Errorf("start COM: %w", err)
		}
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return fmt.Errorf("create shell object: %w", err)
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return fmt.Errorf("shell object: %w", err)
	}
	defer shell.Release()
	v, err := oleutil.CallMethod(shell, "CreateShortcut", path)
	if err != nil {
		return fmt.Errorf("create shortcut: %w", err)
	}
	link := v.ToIDispatch()
	defer link.Release()
	for name, value := range map[string]string{
		"TargetPath":       target,
		"WorkingDirectory": filepath.Dir(target),
		"IconLocation":     target + ",0",
		"Description":      "Your Olsyn Drive on this PC",
	} {
		if _, err := oleutil.PutProperty(link, name, value); err != nil {
			return fmt.Errorf("set shortcut %s: %w", name, err)
		}
	}
	if _, err := oleutil.CallMethod(link, "Save"); err != nil {
		return fmt.Errorf("save shortcut %s: %w", path, err)
	}
	return nil
}

// RemoveStartMenuShortcut removes the Start menu entry; a missing one is fine.
func RemoveStartMenuShortcut() error {
	path, err := startMenuShortcut()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove start menu shortcut: %w", err)
	}
	return nil
}

// InstalledVersion reads the version recorded by the last install (the
// uninstall entry's DisplayVersion); "" when nothing is installed.
func InstalledVersion() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, uninstallKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("open uninstall entry: %w", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue("DisplayVersion")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return "", fmt.Errorf("read installed version: %w", err)
	}
	return v, nil
}

// StopProgram ends every other process of this user started from exe (the
// installed copy being replaced) and waits for them to exit. Call it only
// after the agent has been asked to quit cleanly: what is left is the
// desktop app, which keeps nothing that is not already saved.
func StopProgram(ctx context.Context, exe string) error {
	want, err := filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", exe, err)
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	defer windows.CloseHandle(snap)
	self := uint32(os.Getpid())
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var handles []windows.Handle
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()
	var errs []error
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ProcessID == self || entry.ProcessID == 0 {
			continue
		}
		if !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), filepath.Base(want)) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, entry.ProcessID)
		if err != nil {
			continue // another user's process, or already gone
		}
		if !strings.EqualFold(processImage(h), want) {
			windows.CloseHandle(h)
			continue
		}
		handles = append(handles, h)
		if err := windows.TerminateProcess(h, 1); err != nil {
			errs = append(errs, fmt.Errorf("stop process %d: %w", entry.ProcessID, err))
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, h := range handles {
		for {
			ev, err := windows.WaitForSingleObject(h, 200)
			if err != nil || ev == windows.WAIT_OBJECT_0 {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("wait for old processes: %w", ctx.Err())
			}
			if time.Now().After(deadline) {
				errs = append(errs, errors.New("an old Olsyn Drive process did not exit"))
				break
			}
		}
	}
	return errors.Join(errs...)
}

func processImage(h windows.Handle) string {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
