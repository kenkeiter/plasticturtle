//go:build integration && darwin

package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file covers the one part of plasticturtle shell that only appears when its stdin is
// a real terminal: PTY negotiation. Everything in integration_test.go drives pt
// through a pipe, which requests no PTY at all, so none of it can see a broken
// TERM or a wrong erase character.

// openPTY allocates a pseudo-terminal pair. Darwin grants and unlocks the
// replica through TIOCPTY* rather than posix_openpt, which golang.org/x/sys
// does not expose here.
func openPTY(t *testing.T) (master, replica *os.File) {
	t.Helper()

	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	fail := func(what string, err error) {
		m.Close()
		t.Skipf("%s: %v", what, err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYGRANT, 0); err != nil {
		fail("TIOCPTYGRANT", err)
	}
	if err := unix.IoctlSetInt(int(m.Fd()), unix.TIOCPTYUNLK, 0); err != nil {
		fail("TIOCPTYUNLK", err)
	}
	var buf [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		fail("TIOCPTYGNAME", errno)
	}
	name := string(buf[:bytes.IndexByte(buf[:], 0)])

	// O_NOCTTY: the test process must not adopt this terminal as its own, which
	// would redirect its signals. The child claims it instead, via Setctty.
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

// hostOnlyTerm builds a terminfo entry that exists on this host and cannot
// exist in the guest, and returns its name and the database directory holding
// it.
//
// Synthesised rather than borrowed from whatever terminal is running the test:
// the negotiation's whole job is the case where the host has an entry the guest
// does not, and a made-up name is the only way to guarantee that case on any
// machine. It is a copy of xterm-256color under a different name, so a guest
// that ends up using it gets a terminal that genuinely works.
func hostOnlyTerm(t *testing.T) (term, dir string) {
	t.Helper()

	term = "pt-integration-term"
	src, err := exec.Command("infocmp", "-x", "xterm-256color").Output()
	if err != nil {
		t.Skipf("host cannot describe xterm-256color: %v", err)
	}

	// Rename the entry by rewriting its header — the first line that is not a
	// comment — and drop the aliases, so nothing but our name resolves to it.
	lines := strings.Split(string(src), "\n")
	renamed := false
	for i, line := range lines {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		lines[i] = term + "|plastic turtle integration terminal,"
		renamed = true
		break
	}
	if !renamed {
		t.Skip("infocmp produced no entry header to rename")
	}

	dir = filepath.Join(t.TempDir(), "terminfo")
	tic := exec.Command("tic", "-x", "-o", dir, "-")
	tic.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	if out, err := tic.CombinedOutput(); err != nil {
		t.Skipf("host tic refused the synthesised entry: %v\n%s", err, out)
	}

	// Prove the premise before relying on it.
	check := exec.Command("infocmp", term)
	check.Env = append(os.Environ(), "TERMINFO="+dir)
	if err := check.Run(); err != nil {
		t.Skipf("synthesised entry %s is not resolvable on the host: %v", term, err)
	}
	return term, dir
}

// shellOnPTY runs plasticturtle shell with a real terminal on its stdin, so that it
// negotiates a PTY, and feeds script to the guest's shell through that
// terminal. It returns everything the terminal saw.
func (w *world) shellOnPTY(term, terminfoDir, script string) string {
	w.t.Helper()

	master, replica := openPTY(w.t)

	// An erase character the guest would never pick on its own: the default is
	// ^?, so seeing ^H in the guest can only mean the host's setting was
	// forwarded.
	tio, err := unix.IoctlGetTermios(int(replica.Fd()), unix.TIOCGETA)
	if err != nil {
		w.t.Fatalf("get termios: %v", err)
	}
	tio.Cc[unix.VERASE] = 0x08
	if err := unix.IoctlSetTermios(int(replica.Fd()), unix.TIOCSETA, tio); err != nil {
		w.t.Fatalf("set termios: %v", err)
	}

	c := exec.Command(w.bin, "shell", w.project)
	c.Env = append(os.Environ(),
		"XDG_STATE_HOME="+w.state,
		"TERM="+term,
		"TERMINFO="+terminfoDir,
	)
	c.Stdin, c.Stdout, c.Stderr = replica, replica, replica
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	// The terminal is drained continuously: a PTY has a small buffer, and a
	// session nobody is reading from wedges rather than failing.
	var (
		mu       sync.Mutex
		seen     bytes.Buffer
		lastByte time.Time
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				mu.Lock()
				seen.Write(chunk)
				lastByte = time.Now()
				mu.Unlock()
				answerQueries(master, chunk)
			}
			if err != nil {
				return
			}
		}
	}()

	if err := c.Start(); err != nil {
		w.t.Fatalf("start plasticturtle shell: %v", err)
	}
	exited := make(chan struct{})
	go func() { defer close(exited); _ = c.Wait() }()

	transcript := func() string {
		mu.Lock()
		defer mu.Unlock()
		return seen.String()
	}
	abandon := func(format string, args ...any) {
		_ = c.Process.Kill()
		<-exited
		w.t.Fatalf(format+"\nterminal saw:\n%s\n\nsupervisor.log:\n%s",
			append(args, transcript(), w.supervisorLog())...)
	}

	// Nothing may be typed until the guest's shell is there to read it. pt
	// starts copying stdin only once the session is up, and bytes written
	// before that are echoed by the local terminal and then discarded.
	//
	// Readiness is inferred from the terminal going quiet rather than from a
	// prompt string: the boot spinner writes continuously, so silence means the
	// prompt is drawn, and matching a prompt would mean assuming the guest's
	// shell already renders correctly — which is the thing under test.
	if !w.waitForQuiet(&mu, &lastByte, exited) {
		abandon("no guest prompt within %s", bootBudget)
	}

	// ^U first, and then wait for quiet again: a query answered slightly after
	// the guest's shell gave up waiting for it is left sitting on the command
	// line, where it would be run as the first word of the script.
	if _, err := master.WriteString("\x15\r"); err != nil {
		w.t.Fatalf("clear the guest's command line: %v", err)
	}
	if !w.waitForQuiet(&mu, &lastByte, exited) {
		abandon("guest shell never settled after clearing its command line")
	}
	if _, err := master.WriteString(script); err != nil {
		w.t.Fatalf("write to terminal: %v", err)
	}

	select {
	case <-exited:
	case <-time.After(2 * time.Minute):
		abandon("plasticturtle shell never exited after the script was typed")
	}

	// Let the drain finish what is still in the buffer.
	_ = replica.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return transcript()
}

// answerQueries replies to the terminal queries a guest shell blocks on.
//
// zsh asks where the cursor is (DSR) to decide whether the last command left a
// partial line, and asks for the background colour (OSC 11) to pick its
// palette. A real terminal answers both; a pipe that only records output does
// not, and the shell waits — which looks exactly like a session that hung.
// Replies go out in the order the queries arrived, because the shell reads them
// in that order too.
func answerQueries(master *os.File, chunk []byte) {
	for i := range chunk {
		switch {
		case bytes.HasPrefix(chunk[i:], []byte("\x1b]11;?")):
			_, _ = master.WriteString("\x1b]11;rgb:0000/0000/0000\x1b\\")
		case bytes.HasPrefix(chunk[i:], []byte("\x1b[6n")):
			_, _ = master.WriteString("\x1b[1;1R")
		}
	}
}

// waitForQuiet blocks until the terminal has produced something and then gone
// silent for long enough to be a drawn prompt rather than a pause in the boot
// spinner. It reports false if the boot budget ran out, or if pt exited first.
func (w *world) waitForQuiet(mu *sync.Mutex, lastByte *time.Time, exited <-chan struct{}) bool {
	w.t.Helper()

	const quiet = 3 * time.Second
	deadline := time.Now().Add(bootBudget)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return false
		case <-time.After(500 * time.Millisecond):
		}
		mu.Lock()
		last := *lastByte
		mu.Unlock()
		if !last.IsZero() && time.Since(last) >= quiet {
			return true
		}
	}
	return false
}

// TestPTYSessionTermAndEraseReachTheGuest is the real-VM proof for the
// backspace bug.
//
// Two independent things have to be true inside the guest for line editing to
// work, and neither was before: the guest must be able to resolve the TERM the
// session requested — otherwise its shell has no way to move the cursor back
// over a deleted character — and its PTY must erase on the character the user's
// keyboard actually sends.
func TestPTYSessionTermAndEraseReachTheGuest(t *testing.T) {
	w := newWorld(t, basicConfig)
	term, dir := hostOnlyTerm(t)

	// Reported one per line with markers, because a shell prompt writes
	// whatever it likes around them.
	// Every marker's value has to come from the guest at run time, never from
	// the text of the command. The guest's shell echoes what is typed into it,
	// so a script containing the literal string being asserted on would make
	// the assertion pass without the guest agreeing.
	//
	// awk over the cchars rather than a regex, because stty prints "werase"
	// too and a greedy match for "erase" finds the wrong one.
	const script = `
printf 'pt-term=%s\n' "$TERM"
infocmp "$TERM" >/dev/null 2>&1; printf 'pt-terminfo-status=%s\n' "$?"
stty -a | tr ';' '\n' | awk '$1=="erase"{print "pt-erase=" $3}'
exit
`
	out := w.shellOnPTY(term, dir, script)

	for _, want := range []struct{ marker, why string }{
		{"pt-term=" + term, "the session fell back to a plainer TERM instead of teaching the guest the host's"},
		{"pt-terminfo-status=0", "the guest cannot resolve the TERM it was given — its shell has no way to move the cursor, so backspace will not erase"},
		{"pt-erase=^H", "the host's erase character was not forwarded; the guest is erasing on a character the keyboard does not send"},
	} {
		if !strings.Contains(out, want.marker) {
			t.Errorf("%s\nexpected %q in the session output:\n%s", want.why, want.marker, out)
		}
	}
}
