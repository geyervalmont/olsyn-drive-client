//go:build windows

package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// On Windows the token is encrypted with DPAPI in user scope
// (CryptProtectData without CRYPTPROTECT_LOCAL_MACHINE): only this Windows
// user, on this computer, can decrypt it, even if the file is copied off.
// It's called from crypt32.dll directly, so no cgo and no new dependency.

func tokenPath(dir string) string { return filepath.Join(dir, "token.dpapi") }

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	protectData   = crypt32.NewProc("CryptProtectData")
	unprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	localFree     = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	n    uint32
	data *byte
}

func blob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{n: uint32(len(b)), data: &b[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.n)
	copy(out, unsafe.Slice(b.data, b.n))
	return out
}

const cryptProtectUIForbidden = 0x1

var entropy = []byte("olsyn-drive token v1")

func saveToken(dir, tok string) error {
	var out dataBlob
	r, _, err := protectData.Call(uintptr(unsafe.Pointer(blob([]byte(tok)))), 0,
		uintptr(unsafe.Pointer(blob(entropy))), 0, 0, cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return fmt.Errorf("CryptProtectData: %w", err)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.data)))
	return writePrivate(tokenPath(dir), out.bytes())
}

func loadToken(dir string) (string, error) {
	enc, err := os.ReadFile(tokenPath(dir))
	if err != nil {
		return "", err
	}
	var out dataBlob
	r, _, err := unprotectData.Call(uintptr(unsafe.Pointer(blob(enc))), 0,
		uintptr(unsafe.Pointer(blob(entropy))), 0, 0, cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return "", fmt.Errorf("the stored sign-in belongs to another Windows user or computer; run olsyn-drive login (%v)", err)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.data)))
	return strings.TrimSpace(string(out.bytes())), nil
}
