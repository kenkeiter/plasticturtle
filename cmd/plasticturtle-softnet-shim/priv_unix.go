package main

import (
	"fmt"
	"os"
	"syscall"
)

// dropPrivileges permanently drops the effective uid/gid back to the real user.
// It sets the group first (a uid without privilege can no longer change gid) and
// verifies the drop stuck, refusing to continue as root if it did not.
//
// It runs once the vmnet interface exists: the interface belongs to the process,
// not to the uid that created it, so everything after this point — the policy
// file, every guest frame — is handled as the invoking user.
func dropPrivileges() error {
	rgid := os.Getgid()
	ruid := os.Getuid()
	if os.Geteuid() == ruid {
		return nil // nothing to drop
	}
	if err := syscall.Setgid(rgid); err != nil {
		return fmt.Errorf("setgid(%d): %w", rgid, err)
	}
	if err := syscall.Setuid(ruid); err != nil {
		return fmt.Errorf("setuid(%d): %w", ruid, err)
	}
	if os.Geteuid() != ruid || os.Getuid() != ruid {
		return fmt.Errorf("privilege drop did not stick (euid %d ruid %d)", os.Geteuid(), os.Getuid())
	}
	return nil
}
