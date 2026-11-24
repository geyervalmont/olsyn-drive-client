package platform

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	credMaxBlobSize         = 5 * 512
)

// credential mirrors CREDENTIALW.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// winCredStore is the Windows Credential Manager, through advapi32.
type winCredStore struct {
	credRead   *windows.LazyProc
	credWrite  *windows.LazyProc
	credDelete *windows.LazyProc
	credFree   *windows.LazyProc
}

// NewCredentialStore returns the Windows Credential Manager store. Secrets are
// generic credentials, persisted for this user on this machine (not roaming).
func NewCredentialStore() CredentialStore {
	advapi := windows.NewLazySystemDLL("advapi32.dll")
	return &winCredStore{
		credRead:   advapi.NewProc("CredReadW"),
		credWrite:  advapi.NewProc("CredWriteW"),
		credDelete: advapi.NewProc("CredDeleteW"),
		credFree:   advapi.NewProc("CredFree"),
	}
}

func (s *winCredStore) Get(target string) ([]byte, error) {
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, fmt.Errorf("credential name: %w", err)
	}
	var c *credential
	r, _, e := s.credRead.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		if errors.Is(e, windows.ERROR_NOT_FOUND) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read credential: %w", e)
	}
	defer s.credFree.Call(uintptr(unsafe.Pointer(c)))
	if c.CredentialBlobSize == 0 || c.CredentialBlob == nil {
		return []byte{}, nil
	}
	blob := unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize)
	return append([]byte(nil), blob...), nil
}

func (s *winCredStore) Set(target string, secret []byte) error {
	if len(secret) > credMaxBlobSize {
		return fmt.Errorf("secret is %d bytes, the credential store holds at most %d", len(secret), credMaxBlobSize)
	}
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("credential name: %w", err)
	}
	user, _ := windows.UTF16PtrFromString("olsyn-drive")
	c := credential{
		Type:               credTypeGeneric,
		TargetName:         t,
		CredentialBlobSize: uint32(len(secret)),
		Persist:            credPersistLocalMachine,
		UserName:           user,
	}
	if len(secret) > 0 {
		c.CredentialBlob = &secret[0]
	}
	r, _, e := s.credWrite.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r == 0 {
		return fmt.Errorf("write credential: %w", e)
	}
	return nil
}

// CheckWritable writes and removes a throwaway credential, which fails with
// ERROR_NO_SUCH_LOGON_SESSION where Credential Manager is unavailable.
func (s *winCredStore) CheckWritable() error {
	const probe = "Olsyn Drive/write-check"
	if err := s.Set(probe, []byte{1}); err != nil {
		return fmt.Errorf("the Windows credential store cannot keep a sign-in in this session: %w", err)
	}
	if err := s.Delete(probe); err != nil {
		return fmt.Errorf("the Windows credential store cannot keep a sign-in in this session: %w", err)
	}
	return nil
}

func (s *winCredStore) Delete(target string) error {
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("credential name: %w", err)
	}
	r, _, e := s.credDelete.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(e, windows.ERROR_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("delete credential: %w", e)
	}
	return nil
}
