package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// trustedSoftnetPaths are the only locations a privileged shim will exec the
// real softnet from. A SUID-root process must never take an executable path from
// the environment or $PATH: doing so would let any local user point it at
// /bin/sh and gain root. These are the standard Homebrew locations softnet ships
// to.
var trustedSoftnetPaths = []string{
	"/opt/homebrew/bin/softnet",
	"/usr/local/bin/softnet",
}

// resolveRealSoftnet finds the genuine softnet binary to exec.
//
// When privileged (running SUID, euid != ruid), it ignores the environment
// entirely and returns the first trusted path that is a root-owned, non-
// world/group-writable regular file — the standard hardening for a setuid
// helper. When unprivileged (developer builds, tests), PT_REAL_SOFTNET may
// override, since there is no privilege to escalate.
func resolveRealSoftnet(privileged bool, getenv func(string) string, stat func(string) (os.FileInfo, error)) (string, error) {
	if !privileged {
		if p := getenv("PT_REAL_SOFTNET"); p != "" {
			return p, nil
		}
	}
	var tried []string
	for _, p := range trustedSoftnetPaths {
		// Resolve symlinks (Homebrew's bin/softnet points into Cellar) and then
		// verify the *target* is safe, so a compromised symlink cannot redirect
		// us to an attacker-writable file.
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		fi, err := stat(real)
		if err != nil {
			continue
		}
		if err := safeRootExecutable(fi); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", real, err))
			continue
		}
		return real, nil
	}
	if len(tried) > 0 {
		return "", fmt.Errorf("softnet found but not safe to run as root (%v); re-run `pt setup-firewall`", tried)
	}
	return "", fmt.Errorf("no softnet found in %v; install it and re-run `pt setup-firewall`", trustedSoftnetPaths)
}
