//go:build !darwin

package sshx

import "golang.org/x/crypto/ssh"

// localModes has no implementation off Darwin, which is the only host platform
// pt supports — Tart is macOS/arm64 only. Reporting false makes Interactive
// send the minimal mode set, which is what this package sent everywhere before
// termios forwarding existed.
func localModes(int) (ssh.TerminalModes, bool) { return nil, false }
