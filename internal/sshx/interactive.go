package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// transportExitCode is what Interactive reports when the session failed rather
// than the remote command: ssh(1) uses 255 for its own errors, and pt shell
// mirrors that so a script can tell "the VM said no" from "we never got in".
const transportExitCode = 255

// fallbackModes is what the guest PTY gets when the local terminal's settings
// cannot be read. ECHO is the one that matters: with the local terminal in raw
// mode, nothing echoes what the user types unless the guest does.
var fallbackModes = ssh.TerminalModes{
	ssh.ECHO:          1,
	ssh.TTY_OP_ISPEED: 14400,
	ssh.TTY_OP_OSPEED: 14400,
}

// InteractiveOption configures one Interactive session.
type InteractiveOption func(*interactiveOpts)

type interactiveOpts struct {
	status *StatusLine
}

// WithStatusLine reserves the terminal's bottom row for status, for as long
// as the session lasts. It requires a real terminal at least three rows tall;
// anything less and the option is silently ignored — a session without a
// banner beats no session.
func WithStatusLine(status *StatusLine) InteractiveOption {
	return func(o *interactiveOpts) { o.status = status }
}

// Interactive runs command on the guest with a PTY attached to tty, mirroring
// its size, its terminal settings and — after negotiating one the guest can
// resolve — its TERM, forwarding SIGWINCH on resize, and putting the local
// terminal in raw mode for the duration.
//
// It returns the remote command's exit status, which pt shell exits with. Raw
// mode is always restored, including on panic: leaving a user's terminal in
// raw mode is the worst failure this tool can produce.
func (c *Client) Interactive(ctx context.Context, command string, tty *os.File, opts ...InteractiveOption) (exitCode int, err error) {
	var cfg interactiveOpts
	for _, o := range opts {
		o(&cfg)
	}
	sess, err := c.conn.NewSession()
	if err != nil {
		return transportExitCode, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	// A PTY is requested only for a real terminal. Asking for one when stdout
	// is a pipe would give the guest a terminal it can't size and merge stderr
	// into stdout for a caller that is parsing it.
	isTTY := tty != nil && term.IsTerminal(int(tty.Fd()))

	// bar is non-nil only when a status line is both requested and possible;
	// every piece of special handling below keys off it.
	var bar *statusBar

	if isTTY {
		fd := int(tty.Fd())

		// Both of these happen before raw mode, and must. localModes has to see
		// the user's real settings rather than the ones we are about to impose,
		// and negotiateTerm is a round-trip to the guest that has no business
		// running with the local terminal already raw.
		modes, ok := localModes(fd)
		if !ok {
			modes = fallbackModes
		}
		termName := c.negotiateTerm(ctx, os.Getenv("TERM"))

		// The restore is deferred before anything else can fail, so it runs on
		// every path out of this function including a panic unwinding through
		// it. Everything below this point may fail freely.
		state, rawErr := term.MakeRaw(fd)
		if rawErr != nil {
			return transportExitCode, fmt.Errorf("raw mode: %w", rawErr)
		}
		defer func() { _ = term.Restore(fd, state) }()

		width, height, sizeErr := term.GetSize(fd)
		if sizeErr != nil || width <= 0 || height <= 0 {
			width, height = 80, 24
		}

		// The status line claims the bottom row, so the guest is told the
		// terminal is one row shorter. Below three rows there is no useful
		// split, so the option is dropped rather than fought for.
		guestHeight := height
		if cfg.status != nil && cfg.status.Render != nil && height >= 3 {
			bar = &statusBar{out: tty, render: cfg.status.Render, width: width, height: height}
			guestHeight = bar.guestHeight()
		}

		if err := sess.RequestPty(termName, guestHeight, width, modes); err != nil {
			return transportExitCode, fmt.Errorf("request pty: %w", err)
		}

		if bar != nil {
			// Deferred while raw mode's own restore is already pending, so on
			// the way out stop runs first (LIFO), while the sequences it
			// emits still mean what they say.
			bar.start()
			cfg.status.attach(bar)
			defer func() {
				cfg.status.detach()
				bar.stop()
			}()
		}

		stopWinch := forwardResizes(sess, fd, bar)
		defer stopWinch()
	}

	if tty != nil {
		// With a PTY the guest merges stderr into the same stream anyway;
		// pointing both at the terminal keeps the non-PTY case ordered too.
		var out io.Writer = tty
		if bar != nil {
			out = &vtFilter{bar: bar}
		}
		sess.Stdout, sess.Stderr = out, out
	} else {
		sess.Stdout, sess.Stderr = os.Stdout, os.Stderr
	}

	if tty != nil {
		// Stdin is copied through an explicit pipe rather than sess.Stdin
		// because Session.Wait waits for its copier to finish, and a copier
		// reading a terminal never sees EOF — the session would hang forever
		// after the remote shell exited. The cost is that this goroutine
		// outlives the session, parked on a read; the process it belongs to
		// exits moments later.
		in, err := sess.StdinPipe()
		if err != nil {
			return transportExitCode, fmt.Errorf("stdin pipe: %w", err)
		}
		go func() {
			_, _ = io.Copy(in, tty)
			_ = in.Close()
		}()
	} else {
		// Without a terminal, stdin must still reach the guest: `pt shell <
		// script`, a heredoc, and anything driving pt from CI all depend on it.
		// Leaving it unset gives the remote shell immediate EOF, so it exits 0
		// having run nothing — a silent success, which is the worst outcome
		// available.
		//
		// The hang that motivates the pipe above cannot happen here: a pipe or
		// a regular file does reach EOF, so Session.Wait's copier finishes.
		sess.Stdin = os.Stdin
	}

	// An empty command means "whatever the guest gives a bare login", which on
	// the wire is a shell request rather than an exec of the empty string.
	start := sess.Start
	if command == "" {
		start = func(string) error { return sess.Shell() }
	}
	if err := start(command); err != nil {
		return transportExitCode, fmt.Errorf("start remote command: %w", err)
	}

	// Cancellation closes the session; the remote side sees the channel drop
	// and its shell gets a SIGHUP, which is what a hung-up ssh does.
	stopCancel := context.AfterFunc(ctx, func() { _ = sess.Close() })
	waitErr := sess.Wait()
	stopCancel()

	code, ok := remoteExitCode(waitErr)
	if ok {
		return code, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return transportExitCode, fmt.Errorf("session cancelled: %w", ctxErr)
	}
	return transportExitCode, fmt.Errorf("session: %w", waitErr)
}

// remoteExitCode maps a Session.Wait result to a shell exit status. The second
// result is false when the failure was ours (transport, cancellation) rather
// than the remote command's, which the caller must not report as an exit code.
func remoteExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	if sig := exitErr.Signal(); sig != "" {
		// A shell killed by a signal reports 128+signum; without this a
		// ^C-killed remote command would look like a clean exit 0.
		if num, known := signalNumbers[ssh.Signal(sig)]; known {
			return 128 + int(num), true
		}
		return transportExitCode, true
	}
	return exitErr.ExitStatus(), true
}

// signalNumbers maps the names SSH puts on the wire to local signal numbers.
// The numbers are taken from the host's own syscall package rather than
// hard-coded, because they differ across Unixes (USR1 is 30 on Darwin, 10 on
// Linux) and the code we produce is consumed by the host's shell.
var signalNumbers = map[ssh.Signal]syscall.Signal{
	ssh.SIGABRT: syscall.SIGABRT,
	ssh.SIGALRM: syscall.SIGALRM,
	ssh.SIGFPE:  syscall.SIGFPE,
	ssh.SIGHUP:  syscall.SIGHUP,
	ssh.SIGILL:  syscall.SIGILL,
	ssh.SIGINT:  syscall.SIGINT,
	ssh.SIGKILL: syscall.SIGKILL,
	ssh.SIGPIPE: syscall.SIGPIPE,
	ssh.SIGQUIT: syscall.SIGQUIT,
	ssh.SIGSEGV: syscall.SIGSEGV,
	ssh.SIGTERM: syscall.SIGTERM,
	ssh.SIGUSR1: syscall.SIGUSR1,
	ssh.SIGUSR2: syscall.SIGUSR2,
}

// forwardResizes mirrors local terminal resizes into the remote PTY until the
// returned stop function is called. Without it, a window resized mid-session
// leaves the guest's shell drawing to the old geometry for the rest of the
// session — there is no other way for it to learn.
//
// With a status bar, the bar re-anchors first and the guest is told the
// reduced height; the other order would have the guest paint into a row the
// bar still thought it owned.
func forwardResizes(sess *ssh.Session, fd int, bar *statusBar) (stop func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-winch:
				width, height, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				if bar != nil {
					height = bar.resize(width, height)
				}
				// A failed window-change means the session is going away; the
				// Wait in Interactive will report it.
				_ = sess.WindowChange(height, width)
			case <-done:
				return
			}
		}
	}()

	var once bool
	return func() {
		if once {
			return
		}
		once = true
		signal.Stop(winch)
		close(done)
	}
}
