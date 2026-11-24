package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// DetectDriver finds WinFsp: the InstallDir registry value (WinFsp's installer
// writes it in the 32-bit registry view), the DLL for this architecture, and
// that DLL's file version.
func DetectDriver() (DriverInfo, error) {
	info := DriverInfo{Name: "winfsp", Needed: true}
	dir, err := winfspInstallDir()
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			info.Others = otherProviders()
			return info, nil
		}
		return info, err
	}
	info.InstallDir = dir
	dll := filepath.Join(dir, "bin", winfspDLLName())
	if _, err := os.Stat(dll); err != nil {
		return info, nil // registry left behind by an uninstall: treat as missing
	}
	v, err := fileVersion(dll)
	if err != nil {
		return info, fmt.Errorf("read winfsp version: %w", err)
	}
	if v == "" {
		return info, errors.New("winfsp has no version")
	}
	info.Installed = true
	info.Version = v
	info.Others = otherProviders()
	return info, nil
}

// otherProviders lists other file system providers registered with Windows'
// network provider order. Dokany registers "Dokan..."; it coexists with
// WinFsp, so it is only reported.
func otherProviders() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\NetworkProvider\Order`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	order, _, err := k.GetStringValue("ProviderOrder")
	if err != nil {
		return nil
	}
	return OtherProvidersIn(order)
}

func winfspInstallDir() (string, error) {
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		dir, _, err := k.GetStringValue("InstallDir")
		k.Close()
		if err == nil && dir != "" {
			return dir, nil
		}
	}
	return "", registry.ErrNotExist
}

func winfspDLLName() string {
	switch runtime.GOARCH {
	case "arm64":
		return "winfsp-a64.dll"
	case "386":
		return "winfsp-x86.dll"
	default:
		return "winfsp-x64.dll"
	}
}

// fileVersion reads VS_FIXEDFILEINFO.FileVersion as "a.b.c".
func fileVersion(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return "", fmt.Errorf("version info size: %w", err)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("version info: %w", err)
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &n); err != nil {
		return "", fmt.Errorf("query fixed version: %w", err)
	}
	if fixed == nil || n == 0 {
		return "", errors.New("no fixed version info")
	}
	return fmt.Sprintf("%d.%d.%d", fixed.FileVersionMS>>16, fixed.FileVersionMS&0xffff, fixed.FileVersionLS>>16), nil
}

// shellExecuteInfo mirrors SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	msiRebootRequired     = 3010
	msiUserExit           = 1602
)

// InstallDriver runs the WinFsp MSI elevated (one UAC prompt) and waits for it.
// The installer is first copied into workDir, then the copy must match
// wantSHA256 and carry a valid Authenticode signature before it is run, so a
// file swapped after the check is never executed. progress (may be nil)
// hears the DriverStep values as they begin. A declined prompt is
// ErrDeclined; an install that needs Windows restarted is ErrRestartNeeded.
func InstallDriver(ctx context.Context, installer, wantSHA256, workDir string, progress func(step string)) error {
	if wantSHA256 == "" {
		return errors.New("installer hash is required")
	}
	if progress == nil {
		progress = func(string) {}
	}
	progress(DriverStepPreparing)
	staged, err := stageVerified(installer, wantSHA256, workDir)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	if err := verifyAuthenticode(staged); err != nil {
		return err
	}
	sysDir, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("find system folder: %w", err)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(filepath.Join(sysDir, "msiexec.exe"))
	params, err := windows.UTF16PtrFromString(`/i "` + staged + `" /passive /norestart`)
	if err != nil {
		return fmt.Errorf("encode parameters: %w", err)
	}
	sei := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        windows.SW_SHOWNORMAL,
	}
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	progress(DriverStepApproval)
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	if r, _, e := proc.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(e, windows.ERROR_CANCELLED) {
			return ErrDeclined
		}
		return fmt.Errorf("start driver installer: %w", e)
	}
	if sei.hProcess == 0 {
		return errors.New("driver installer started without a process handle")
	}
	defer windows.CloseHandle(sei.hProcess)
	// ShellExecuteEx returns once the prompt was accepted and msiexec runs.
	progress(DriverStepInstalling)
	for {
		ev, err := windows.WaitForSingleObject(sei.hProcess, 500)
		if err != nil {
			return fmt.Errorf("wait for driver installer: %w", err)
		}
		if ev == windows.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("wait for driver installer: %w", ctx.Err())
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(sei.hProcess, &code); err != nil {
		return fmt.Errorf("driver installer exit code: %w", err)
	}
	switch code {
	case 0:
		return nil
	case msiRebootRequired:
		return ErrRestartNeeded
	case msiUserExit:
		return ErrDeclined
	default:
		return fmt.Errorf("driver installer failed with code %d", code)
	}
}

func stageVerified(src, wantSHA256, workDir string) (string, error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", workDir, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open driver installer: %w", err)
	}
	defer in.Close()
	dst := filepath.Join(workDir, fmt.Sprintf("winfsp-%d.msi", time.Now().UnixNano()))
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("stage driver installer: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("copy driver installer: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		os.Remove(dst)
		return "", fmt.Errorf("driver installer hash %s does not match the expected %s", got, wantSHA256)
	}
	return dst, nil
}

func verifyAuthenticode(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}
	fi := &windows.WinTrustFileInfo{FilePath: p}
	fi.Size = uint32(unsafe.Sizeof(*fi))
	data := &windows.WinTrustData{
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     windows.WTD_CHOICE_FILE,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(fi),
	}
	data.Size = uint32(unsafe.Sizeof(*data))
	verr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	if verr != nil {
		return fmt.Errorf("driver installer signature is not valid: %w", verr)
	}
	return nil
}
