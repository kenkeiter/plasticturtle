package sshx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
)

// defaultTerm is the name used when the host has no TERM, and the fallback when
// the guest cannot be taught the host's. Something is better than the empty
// string, which makes curses programs in the guest refuse to start, and this
// particular name has been in every terminfo database for decades.
const defaultTerm = "xterm-256color"

// terminfoInstallCommand compiles a description read on stdin into the guest
// user's own terminfo database.
//
// Per-user rather than system-wide: ncurses searches $HOME/.terminfo before the
// compiled-in directories, and writing there needs no privilege — which matters
// because the guest login is not necessarily one that can write /usr/share.
// The clone is discarded at teardown, so nothing is left behind on the host.
//
// Two pt shells attaching to the same instance can run this concurrently and
// write the same file. That is left unguarded: the writes are byte-identical,
// both are verified afterwards, and the worst outcome is that one session falls
// back to a plainer TERM inside a VM that is about to be thrown away. A lock
// would cost every session a round-trip to prevent that.
const terminfoInstallCommand = `mkdir -p "$HOME/.terminfo" && tic -x -o "$HOME/.terminfo" - >/dev/null 2>&1`

// terminfoProbeCommand asks the guest whether it can resolve term.
func terminfoProbeCommand(term string) string {
	return "infocmp " + shellQuote(term) + " >/dev/null 2>&1"
}

// negotiateTerm decides what TERM to put in the pty-req for a guest.
//
// Modern terminals ship a terminfo entry of their own — Ghostty, kitty and
// WezTerm all do — and install it only on the host. Forwarding $TERM blindly,
// which is what ssh(1) does, hands the guest a name it cannot resolve; its
// shell then has no cursor-movement capabilities at all, and the first thing
// the user notices is that backspace stops erasing, because the line editor has
// no way to move the cursor back over the character it deleted.
//
// So the name is negotiated instead: ask the guest whether it knows it, and if
// it does not, hand over the host's own compiled description. Only when that
// also fails does the session fall back to defaultTerm — degraded, but never
// broken.
//
// Every failure is silent. This runs between "the VM is up" and "the user has a
// prompt", where a warning about terminfo would be noise printed in front of a
// shell that is about to work fine.
func (c *Client) negotiateTerm(ctx context.Context, hostTerm string) string {
	if hostTerm == "" {
		return defaultTerm
	}
	// The fallback is not worth probing for: no terminfo database ships without
	// it, and if one did there would be nothing better to send instead.
	if hostTerm == defaultTerm {
		return hostTerm
	}

	// Bounded separately from the session's context, which for plasticturtle shell lasts
	// as long as the user's shell does. Everything behind this timeout is an
	// improvement on a working fallback, and none of it is worth delaying a
	// prompt for.
	ctx, cancel := context.WithTimeout(ctx, ptcfg.GuestProbeTimeout)
	defer cancel()

	if c.guestKnowsTerm(ctx, hostTerm) {
		return hostTerm
	}
	entry, err := hostTerminfo(ctx, hostTerm)
	if err != nil {
		return defaultTerm
	}
	if _, err := c.run(ctx, terminfoInstallCommand, bytes.NewReader(entry)); err != nil {
		return defaultTerm
	}
	// Verified rather than assumed. tic exits 0 having stored the description
	// under whatever names its own header declares, which need not include the
	// one we asked about; requesting a TERM the guest still cannot resolve is
	// the exact bug this function exists to prevent.
	if !c.guestKnowsTerm(ctx, hostTerm) {
		return defaultTerm
	}
	return hostTerm
}

// guestKnowsTerm reports whether the guest's terminfo database can resolve term.
func (c *Client) guestKnowsTerm(ctx context.Context, term string) bool {
	_, err := c.run(ctx, terminfoProbeCommand(term), nil)
	return err == nil
}

// hostTerminfo renders the host's compiled description of term back to source,
// in the form tic reads.
//
// -x preserves the user-defined capabilities that are the whole reason a modern
// terminal ships an entry of its own; without it the guest would get a
// description that resolves but has had its distinguishing half removed.
func hostTerminfo(ctx context.Context, term string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "infocmp", "-x", term).Output()
	if err != nil {
		return nil, fmt.Errorf("infocmp %s: %w", term, err)
	}
	// infocmp exits 0 for a name it resolved to nothing useful on some ncurses
	// builds. A description that carries no capability line would compile into
	// an entry as useless as the missing one.
	if !strings.Contains(string(out), ",") {
		return nil, fmt.Errorf("infocmp %s: no usable description", term)
	}
	return out, nil
}
