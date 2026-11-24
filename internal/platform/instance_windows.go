package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// AcquireInstance takes the per-user agent mutex (Local\olsyn-drive-agent-<SID>).
// It returns ErrAlreadyRunning when another agent of this user holds it.
func AcquireInstance(Paths) (func(), error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(`Local\olsyn-drive-agent-` + sid.String())
	if err != nil {
		return nil, fmt.Errorf("mutex name: %w", err)
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			if h != 0 {
				windows.CloseHandle(h)
			}
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("create instance mutex: %w", err)
	}
	return func() { windows.CloseHandle(h) }, nil
}
