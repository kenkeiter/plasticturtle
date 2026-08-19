package main

import (
	"fmt"
	"os"
	"syscall"
)

// safeRootExecutable verifies fi is a regular file owned by root and not
// writable by group or other — the conditions under which execing it as root is
// not a privilege-escalation vector.
func safeRootExecutable(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot stat ownership")
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if st.Uid != 0 {
		return fmt.Errorf("not owned by root (uid %d)", st.Uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("group/world writable (%#o)", fi.Mode().Perm())
	}
	return nil
}

// isPrivileged reports whether the process is running with an effective uid that
// differs from its real uid — i.e. it was entered via the setuid bit. The relay
// drops back to the real uid once the privileged child is spawned.
func isPrivileged() bool {
	return os.Geteuid() == 0 && os.Geteuid() != os.Getuid()
}

// dropPrivileges permanently drops the effective uid/gid back to the real user.
// It sets the group first (a uid without privilege can no longer change gid) and
// verifies the drop stuck, refusing to continue as root if it did not.
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
