package sshx

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sys"
)

func TestDefaultCredentials(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv(EnvUser, "")
		t.Setenv(EnvPassword, "")
		got := DefaultCredentials()
		if got != (Credentials{User: DefaultUser, Password: DefaultPassword}) {
			t.Fatalf("DefaultCredentials() = %+v", got)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Setenv(EnvUser, "ken")
		t.Setenv(EnvPassword, "hunter2")
		got := DefaultCredentials()
		if got != (Credentials{User: "ken", Password: "hunter2"}) {
			t.Fatalf("DefaultCredentials() = %+v", got)
		}
	})
}

// recordingClock reports the durations it was asked to wait for, so the
// backoff schedule can be asserted rather than inferred from wall time.
type recordingClock struct {
	*sys.FakeClock

	mu    sync.Mutex
	waits []time.Duration
}

func (c *recordingClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return c.FakeClock.After(d)
}

func (c *recordingClock) Sleep(d time.Duration) { <-c.After(d) }

func (c *recordingClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.waits...)
}

func TestDialWithRetryBacksOffUntilServerAppears(t *testing.T) {
	// Reserve a loopback port and release it, so the first attempts hit a
	// closed port (refused immediately, no real waiting) and the server can be
	// brought up on that same address later.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	clk := &recordingClock{FakeClock: sys.NewFakeClock(time.Unix(0, 0))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		c   *Client
		err error
	}
	res := make(chan result, 1)
	go func() {
		c, err := DialWithRetry(ctx, addr, testCreds, clk)
		res <- result{c, err}
	}()

	// Four failures, each released by advancing the fake clock. BlockUntil
	// waits for the retry goroutine to register its timer; advancing before
	// that would fire nothing and hang the test.
	const failures = 4
	for i := 0; i < failures; i++ {
		clk.BlockUntil(1)
		clk.Advance(ptcfg.SSHRetryMax)
	}

	// Now let it succeed. The listener is recreated at the same address.
	clk.BlockUntil(1)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("re-listen on %s: %v", addr, err)
	}
	srv, err := newTestServerOn(ln, testCreds)
	if err != nil {
		t.Fatalf("newTestServerOn: %v", err)
	}
	defer srv.Close()
	clk.Advance(ptcfg.SSHRetryMax)

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("DialWithRetry: %v", r.err)
		}
		defer r.c.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("DialWithRetry never returned")
	}

	// Exponential from SSHRetryInitial, clamped at SSHRetryMax. One wait per
	// failed attempt: the four released here plus the one released by the
	// advance that let the server answer.
	want := []time.Duration{
		ptcfg.SSHRetryInitial,
		2 * ptcfg.SSHRetryInitial,
		4 * ptcfg.SSHRetryInitial,
		ptcfg.SSHRetryMax,
		ptcfg.SSHRetryMax,
	}
	got := clk.recorded()
	if len(got) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff waits = %v, want %v", got, want)
		}
	}
}

func TestDialWithRetryStopsWithContext(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	clk := sys.NewFakeClock(time.Unix(0, 0))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := DialWithRetry(ctx, addr, testCreds, clk)
		done <- err
	}()

	clk.BlockUntil(1)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DialWithRetry succeeded against a closed port")
		}
		// The last dial failure must survive into the message; a bare
		// "context canceled" tells a user nothing about their VM.
		if !strings.Contains(err.Error(), "last attempt") {
			t.Fatalf("error lost the underlying cause: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("DialWithRetry ignored context cancellation")
	}
}

func TestProbeTCP(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	if err := ProbeTCP(context.Background(), addr); err != nil {
		t.Fatalf("ProbeTCP against a live listener: %v", err)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	// A closed port must fail well inside the probe timeout: the boot loop
	// polls this and a probe that burns its whole budget would stall it.
	start := time.Now()
	if err := ProbeTCP(context.Background(), addr); err == nil {
		t.Fatal("ProbeTCP succeeded against a closed port")
	}
	if elapsed := time.Since(start); elapsed >= ptcfg.TCPProbeTimeout {
		t.Fatalf("ProbeTCP took %v against a closed port, want well under %v", elapsed, ptcfg.TCPProbeTimeout)
	}
}

func TestLoginCommandShape(t *testing.T) {
	t.Parallel()

	const guestPath = "/Volumes/My Shared Files/project"
	cmd := LoginCommand(guestPath)

	if !strings.Contains(cmd, `cd '/Volumes/My Shared Files/project'`) {
		t.Errorf("guest path is not single-quoted: %s", cmd)
	}
	if !strings.Contains(cmd, "PT_IN_VM_SESSION=1") {
		t.Errorf("session marker missing: %s", cmd)
	}
	if !strings.Contains(cmd, `cd "$HOME"`) {
		t.Errorf("no fallback for guests without the share: %s", cmd)
	}
	// exec last: the user's shell must own the PTY, not a subshell of ours.
	if !strings.HasPrefix(strings.TrimSpace(cmd[strings.LastIndex(cmd, ";")+1:]), "exec ") {
		t.Errorf("command does not end in an exec of the login shell: %s", cmd)
	}

	// An embedded quote must not be able to end the quoted word.
	if got := LoginCommand("/tmp/it's here"); !strings.Contains(got, `'/tmp/it'\''s here'`) {
		t.Errorf("apostrophe not escaped: %s", got)
	}
}

// TestLoginCommandRunsInASpaceyPath executes the preamble under a real /bin/sh
// so the quoting is verified by the thing that will actually parse it, not by
// a string comparison that agrees with the bug.
func TestLoginCommandRunsInASpaceyPath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	project := filepath.Join(base, "My Shared Files", "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A stand-in login shell that reports what the preamble set up for it.
	shell := filepath.Join(base, "fake shell.sh")
	script := "#!/bin/sh\npwd\nprintf '%s\\n' \"$PT_IN_VM_SESSION\"\nprintf '%s\\n' \"$1\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatalf("write shell: %v", err)
	}

	run := func(t *testing.T, path, home string) []string {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", LoginCommand(path))
		cmd.Dir = "/"
		cmd.Env = []string{"SHELL=" + shell, "HOME=" + home, "PATH=/bin:/usr/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run preamble: %v (%s)", err, out)
		}
		return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	}

	lines := run(t, project, base)
	if len(lines) != 3 {
		t.Fatalf("unexpected output %q", lines)
	}
	if resolved, _ := filepath.EvalSymlinks(project); lines[0] != project && lines[0] != resolved {
		t.Errorf("landed in %q, want %q", lines[0], project)
	}
	if lines[1] != "1" {
		t.Errorf("PT_IN_VM_SESSION = %q, want 1", lines[1])
	}
	if lines[2] != "-l" {
		t.Errorf("login shell not invoked with -l: %q", lines[2])
	}

	// Non-macOS guest: the share does not exist, so the user lands in $HOME
	// rather than getting an error before their shell ever starts.
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	lines = run(t, "/Volumes/My Shared Files/project", home)
	if resolved, _ := filepath.EvalSymlinks(home); lines[0] != home && lines[0] != resolved {
		t.Errorf("fallback landed in %q, want %q", lines[0], home)
	}
}
