//go:build !windows

package platform

import "context"

// notImplementedStore is the credential store until the macOS Keychain and the
// Linux Secret Service are wired in.
type notImplementedStore struct{}

// NewCredentialStore returns a store whose every call fails with ErrNotImplemented.
func NewCredentialStore() CredentialStore { return notImplementedStore{} }

func (notImplementedStore) Get(string) ([]byte, error) { return nil, ErrNotImplemented }
func (notImplementedStore) Set(string, []byte) error   { return ErrNotImplemented }
func (notImplementedStore) Delete(string) error        { return ErrNotImplemented }
func (notImplementedStore) CheckWritable() error       { return ErrNotImplemented }

// RegisterURLScheme is not implemented here yet (macOS needs an app bundle).
func RegisterURLScheme(string) error { return ErrNotImplemented }

// UnregisterURLScheme is not implemented here yet.
func UnregisterURLScheme() error { return ErrNotImplemented }

// RegisterUninstall has no meaning outside Windows.
func RegisterUninstall(UninstallEntry) error { return ErrNotImplemented }

// UnregisterUninstall has no meaning outside Windows.
func UnregisterUninstall() error { return ErrNotImplemented }

// DetectDriver reports that no driver is needed (no mount adapter here yet).
func DetectDriver() (DriverInfo, error) { return DriverInfo{Name: "", Needed: false}, nil }

// InstallDriver is Windows only.
func InstallDriver(context.Context, string, string, string) error { return ErrNotImplemented }

// DriveLettersInUse is nil outside Windows: there are no drive letters.
func DriveLettersInUse() (map[string]string, error) { return nil, nil }
