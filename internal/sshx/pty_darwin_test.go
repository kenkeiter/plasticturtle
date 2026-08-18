//go:build darwin

package sshx

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"context"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair so the interactive path can be
// tested against a real terminal. Interactive's riskiest behaviour — raw mode,
// PTY sizing, SIGWINCH — is invisible to any test that only has a pipe, and a
// dozen lines of ioctl here are cheaper than shipping that untested.
//
// Darwin only, which is the only platform pt targets: /dev/ptmx is granted and
// unlocked through the TIOCPTY* ioctls rather than posix_openpt, which
// golang.org/x/sys does not expose on this platform.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()

	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYGRANT, 0); err != nil {
		m.Close()
		t.Skipf("TIOCPTYGRANT: %v", err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYUNLK, 0); err != nil {
		m.Close()
		t.Skipf("TIOCPTYUNLK: %v", err)
	}
	var buf [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		m.Close()
		t.Skipf("TIOCPTYGNAME: %v", errno)
	}
	name := string(buf[:bytes.IndexByte(buf[:], 0)])

	// O_NOCTTY: the test process must not adopt this pty as its controlling
	// terminal, which would redirect its own signals.
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Fatalf("open %s: %v", name, err)
	}

	t.Cleanup(func() {
		s.Close()
		m.Close()
	})
	return m, s
}

func termiosOf(t *testing.T, f *os.File) unix.Termios {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}
	return *tio
}

func setWinsize(t *testing.T, f *os.File, cols, rows uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows}); err != nil {
		t.Fatalf("set winsize: %v", err)
	}
}

// lineReader turns a pty into a channel of lines, because os.File deadlines
// are not dependable on a character device and a blocked test read is
// indistinguishable from a hang.
func lineReader(f *os.File) <-chan string {
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func expectLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-lines:
		if !ok {
			t.Fatalf("terminal closed while waiting for %q", want)
		}
		if got != want {
			t.Fatalf("terminal said %q, want %q", got, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// TestInteractiveOnRealPTY drives the full interactive path against a genuine
// terminal: PTY negotiation, raw mode, SIGWINCH forwarding, stdio and the exit
// status, then asserts the terminal is left exactly as it was found.
func TestInteractiveOnRealPTY(t *testing.T) {
	t.Setenv("TERM", "xterm-mono")

	master, slave := openPTY(t)
	setWinsize(t, slave, 120, 40)
	original := termiosOf(t, slave)

	srv, c := dialTestServer(t)

	ptys := make(chan ptyPayload, 4)
	winches := make(chan windowChangePayload, 8)
	srv.setSessionHooks(
		func(p ptyPayload) bool { ptys <- p; return true },
		func(p windowChangePayload) { winches <- p },
	)

	proceed := make(chan struct{})
	srv.SetExecHandler(func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		_, _ = io.WriteString(stdout, "ready\n")
		// Block until the test has inspected the local terminal and resized
		// it; the session must be live for either check to mean anything.
		<-proceed
		buf := make([]byte, 3)
		_, _ = io.ReadFull(stdin, buf)
		_, _ = io.WriteString(stdout, "got:"+string(buf[:2])+"\n")
		return 7
	})

	lines := lineReader(master)
	done := make(chan int, 1)
	go func() {
		code, err := c.Interactive(context.Background(), "exec $SHELL -l", slave)
		if err != nil {
			t.Errorf("Interactive: %v", err)
		}
		done <- code
	}()

	expectLine(t, lines, "ready")

	// The guest's PTY must mirror the host terminal, or full-screen programs
	// draw to the wrong geometry.
	select {
	case p := <-ptys:
		if p.Term != "xterm-mono" {
			t.Errorf("pty TERM = %q, want xterm-mono", p.Term)
		}
		if p.Columns != 120 || p.Rows != 40 {
			t.Errorf("pty size = %dx%d, want 120x40", p.Columns, p.Rows)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no pty-req reached the guest")
	}

	// Raw mode: canonical input and echo belong to the guest now.
	during := termiosOf(t, slave)
	if during.Lflag&unix.ECHO != 0 || during.Lflag&unix.ICANON != 0 {
		t.Errorf("terminal is not in raw mode during the session (lflag=%#x)", during.Lflag)
	}

	// Resize and make sure it is forwarded.
	setWinsize(t, slave, 100, 30)
	if err := syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatalf("raise SIGWINCH: %v", err)
	}
	deadline := time.After(30 * time.Second)
	for {
		var p windowChangePayload
		select {
		case p = <-winches:
		case <-deadline:
			t.Fatal("resize was never forwarded to the guest")
		}
		if p.Columns == 100 && p.Rows == 30 {
			break
		}
	}

	close(proceed)
	if _, err := master.WriteString("go\n"); err != nil {
		t.Fatalf("write to terminal: %v", err)
	}
	expectLine(t, lines, "got:go")

	select {
	case code := <-done:
		if code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Interactive never returned")
	}

	if restored := termiosOf(t, slave); restored != original {
		t.Fatalf("terminal not restored: got %+v, want %+v", restored, original)
	}
}

// TestInteractiveRestoresTerminalOnFailure covers the deferred restore on the
// error path. It is the same defer that covers a panic, which is the failure
// mode that matters: a tool that leaves a shell in raw mode is worse than one
// that crashes.
func TestInteractiveRestoresTerminalOnFailure(t *testing.T) {
	master, slave := openPTY(t)
	_ = master
	original := termiosOf(t, slave)

	srv, c := dialTestServer(t)
	// Refusing the PTY makes Interactive fail after it has already put the
	// local terminal into raw mode.
	srv.setSessionHooks(func(ptyPayload) bool { return false }, nil)

	if _, err := c.Interactive(context.Background(), "true", slave); err == nil {
		t.Fatal("Interactive succeeded despite a refused pty-req")
	}
	if restored := termiosOf(t, slave); restored != original {
		t.Fatalf("terminal not restored after failure: got %+v, want %+v", restored, original)
	}
}
