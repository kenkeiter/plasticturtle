//go:build darwin

package sshx

import (
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// ccModes maps each SSH control-character opcode to its index in the local
// termios c_cc array. Darwin's indices are not the SSH opcodes and not Linux's
// either, which is why this table exists rather than a cast.
var ccModes = map[uint8]int{
	ssh.VINTR:    unix.VINTR,
	ssh.VQUIT:    unix.VQUIT,
	ssh.VERASE:   unix.VERASE,
	ssh.VKILL:    unix.VKILL,
	ssh.VEOF:     unix.VEOF,
	ssh.VEOL:     unix.VEOL,
	ssh.VEOL2:    unix.VEOL2,
	ssh.VSTART:   unix.VSTART,
	ssh.VSTOP:    unix.VSTOP,
	ssh.VSUSP:    unix.VSUSP,
	ssh.VDSUSP:   unix.VDSUSP,
	ssh.VREPRINT: unix.VREPRINT,
	ssh.VWERASE:  unix.VWERASE,
	ssh.VLNEXT:   unix.VLNEXT,
	ssh.VDISCARD: unix.VDISCARD,
	ssh.VSTATUS:  unix.VSTATUS,
}

// iflagModes, oflagModes, lflagModes map SSH opcodes to the local termios bit
// they carry. Only the flags Darwin defines appear: IUTF8, IUCLC, OLCUC and
// XCASE are Linux-only, and there is nothing on this host to forward for them.
var (
	iflagModes = map[uint8]uint64{
		ssh.IGNPAR:  unix.IGNPAR,
		ssh.PARMRK:  unix.PARMRK,
		ssh.INPCK:   unix.INPCK,
		ssh.ISTRIP:  unix.ISTRIP,
		ssh.INLCR:   unix.INLCR,
		ssh.IGNCR:   unix.IGNCR,
		ssh.ICRNL:   unix.ICRNL,
		ssh.IXON:    unix.IXON,
		ssh.IXANY:   unix.IXANY,
		ssh.IXOFF:   unix.IXOFF,
		ssh.IMAXBEL: unix.IMAXBEL,
	}
	oflagModes = map[uint8]uint64{
		ssh.OPOST:  unix.OPOST,
		ssh.ONLCR:  unix.ONLCR,
		ssh.OCRNL:  unix.OCRNL,
		ssh.ONOCR:  unix.ONOCR,
		ssh.ONLRET: unix.ONLRET,
	}
	lflagModes = map[uint8]uint64{
		ssh.ISIG:    unix.ISIG,
		ssh.ICANON:  unix.ICANON,
		ssh.ECHO:    unix.ECHO,
		ssh.ECHOE:   unix.ECHOE,
		ssh.ECHOK:   unix.ECHOK,
		ssh.ECHONL:  unix.ECHONL,
		ssh.NOFLSH:  unix.NOFLSH,
		ssh.TOSTOP:  unix.TOSTOP,
		ssh.IEXTEN:  unix.IEXTEN,
		ssh.ECHOCTL: unix.ECHOCTL,
		ssh.ECHOKE:  unix.ECHOKE,
		ssh.PENDIN:  unix.PENDIN,
	}
)

// localModes reads fd's terminal settings and renders them as SSH terminal
// modes, the way ssh(1) does.
//
// It must be called before the local terminal is put into raw mode. Raw mode is
// exactly the set of settings the guest must not be given: once the local side
// is raw, the guest's PTY is where canonical input actually happens, so it
// needs the user's real erase key, interrupt character, flow control and echo
// settings — not the defaults its own kernel would pick, which is all it got
// before this existed.
//
// The second result is false if the settings could not be read, which the
// caller answers with a minimal mode set rather than a failed session.
func localModes(fd int) (ssh.TerminalModes, bool) {
	tio, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, false
	}

	modes := make(ssh.TerminalModes, len(ccModes)+len(iflagModes)+len(oflagModes)+len(lflagModes)+6)

	// Control characters pass through unchanged. Darwin spells "no character is
	// bound to this" as _POSIX_VDISABLE == 0xff, which is already the 255 that
	// RFC 4254 reserves for the same meaning — the translation OpenSSH performs
	// on Linux, where the disable value is 0, is a no-op here.
	for op, idx := range ccModes {
		modes[op] = uint32(tio.Cc[idx])
	}

	for op, bit := range iflagModes {
		modes[op] = boolMode(tio.Iflag&bit != 0)
	}
	for op, bit := range oflagModes {
		modes[op] = boolMode(tio.Oflag&bit != 0)
	}
	for op, bit := range lflagModes {
		modes[op] = boolMode(tio.Lflag&bit != 0)
	}

	// Character size is a two-bit field locally and two independent booleans on
	// the wire, so it cannot go through the tables above.
	modes[ssh.CS7] = boolMode(tio.Cflag&unix.CSIZE == unix.CS7)
	modes[ssh.CS8] = boolMode(tio.Cflag&unix.CSIZE == unix.CS8)
	modes[ssh.PARENB] = boolMode(tio.Cflag&unix.PARENB != 0)
	modes[ssh.PARODD] = boolMode(tio.Cflag&unix.PARODD != 0)

	// Baud rates are meaningless for a pty on both ends, but some programs read
	// them to size their output delays, so the real values are forwarded rather
	// than the invented ones this package used to send.
	modes[ssh.TTY_OP_ISPEED] = uint32(tio.Ispeed)
	modes[ssh.TTY_OP_OSPEED] = uint32(tio.Ospeed)

	return modes, true
}

func boolMode(on bool) uint32 {
	if on {
		return 1
	}
	return 0
}
