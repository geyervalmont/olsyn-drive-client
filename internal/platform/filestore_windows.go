package platform

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictToOwner replaces the DACL of a folder or file with one entry, full
// control for the current user, and stops inheritance from the parent.
// Files created in a restricted folder inherit the same (folder ACEs carry
// object and container inherit flags).
func restrictToOwner(path string, dir bool) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	flags := ""
	if dir {
		flags = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;" + sid.String() + ")")
	if err != nil {
		return fmt.Errorf("owner-only security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("owner-only security descriptor: %w", err)
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("set owner-only access on %s: %w", path, err)
	}
	return nil
}
