package blocked

import (
	"errors"
	"syscall"
)

// Windows error codes when a program is refused at start by security
// software or policy.
const (
	errVirusInfected     = syscall.Errno(225)  // ERROR_VIRUS_INFECTED: the file contains a virus or potentially unwanted software
	errVirusDeleted      = syscall.Errno(226)  // ERROR_VIRUS_DELETED: the file was deleted because it contains a virus
	errDisabledByPolicy  = syscall.Errno(1260) // ERROR_ACCESS_DISABLED_BY_POLICY: blocked by group policy
	errAppControlBlocked = syscall.Errno(4551) // ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION: an Application Control policy has blocked this file
)

func refusedBySecurity(err error) bool {
	for _, code := range []syscall.Errno{errVirusInfected, errVirusDeleted, errDisabledByPolicy, errAppControlBlocked} {
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}
