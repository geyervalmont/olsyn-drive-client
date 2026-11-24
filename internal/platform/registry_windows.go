package platform

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath       = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName     = ProductName
	schemeKeyPath    = `Software\Classes\olsyn-drive`
	uninstallKeyPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall\OlsynDrive`
)

// commandLine quotes exe and args the way CreateProcess parses them.
func commandLine(exe string, args []string) string {
	parts := []string{syscall.EscapeArg(exe)}
	for _, a := range args {
		parts = append(parts, syscall.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}

// EnableAutostart starts exe with args at login (HKCU Run key).
func EnableAutostart(exe string, args []string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open run key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(runValueName, commandLine(exe, args)); err != nil {
		return fmt.Errorf("set run value: %w", err)
	}
	return nil
}

// DisableAutostart removes the Run value; a missing one is not an error.
func DisableAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open run key: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(runValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("delete run value: %w", err)
	}
	return nil
}

// AutostartEnabled reports whether the Run value exists.
func AutostartEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open run key: %w", err)
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read run value: %w", err)
	}
	return true, nil
}

// RegisterURLScheme makes olsyn-drive:// links start "exe open-url <link>"
// (HKCU\Software\Classes\olsyn-drive, no admin needed).
func RegisterURLScheme(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, schemeKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create scheme key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue("", "URL:"+ProductName); err != nil {
		return fmt.Errorf("set scheme name: %w", err)
	}
	if err := k.SetStringValue("URL Protocol", ""); err != nil {
		return fmt.Errorf("set url protocol: %w", err)
	}
	if err := setDefault(schemeKeyPath+`\DefaultIcon`, syscall.EscapeArg(exe)+",0"); err != nil {
		return err
	}
	return setDefault(schemeKeyPath+`\shell\open\command`, syscall.EscapeArg(exe)+` open-url "%1"`)
}

// UnregisterURLScheme removes the scheme keys.
func UnregisterURLScheme() error {
	return deleteTree(registry.CURRENT_USER, schemeKeyPath)
}

// RegisterUninstall adds this program to Settings > Apps for the current user.
func RegisterUninstall(e UninstallEntry) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create uninstall key: %w", err)
	}
	defer k.Close()
	values := map[string]string{
		"DisplayName":     e.DisplayName,
		"DisplayVersion":  e.DisplayVersion,
		"Publisher":       e.Publisher,
		"InstallLocation": e.InstallLocation,
		"UninstallString": e.UninstallCommand,
		"DisplayIcon":     e.DisplayIcon,
	}
	for name, v := range values {
		if err := k.SetStringValue(name, v); err != nil {
			return fmt.Errorf("set uninstall %s: %w", name, err)
		}
	}
	for name, v := range map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": e.EstimatedSizeKB} {
		if err := k.SetDWordValue(name, v); err != nil {
			return fmt.Errorf("set uninstall %s: %w", name, err)
		}
	}
	return nil
}

// UnregisterUninstall removes the uninstall entry.
func UnregisterUninstall() error {
	return deleteTree(registry.CURRENT_USER, uninstallKeyPath)
}

func setDefault(path, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer k.Close()
	if err := k.SetStringValue("", value); err != nil {
		return fmt.Errorf("set %s: %w", path, err)
	}
	return nil
}

// deleteTree deletes a key and everything below it; a missing key is fine.
func deleteTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	subs, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return fmt.Errorf("list %s: %w", path, err)
	}
	for _, s := range subs {
		if err := deleteTree(root, path+`\`+s); err != nil {
			return err
		}
	}
	if err := registry.DeleteKey(root, path); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}
