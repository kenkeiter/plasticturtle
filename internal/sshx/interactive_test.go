package sshx

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInteractiveExitStatus covers the path plasticturtle shell's exit code depends on:
// the remote status must come back as a value, not an error, or the shell
// would report a failed session every time a user exits non-zero.
func TestInteractiveExitStatus(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)

	var (
		mu  sync.Mutex
		got string
	)
	srv.SetExecHandler(func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		mu.Lock()
		got = cmd
		mu.Unlock()
		return 42
	})

	want := LoginCommand("/Volumes/My Shared Files/project")
	// A nil tty is the not-a-terminal case: no raw mode, no PTY, no crash.
	code, err := c.Interactive(context.Background(), want, nil)
	if err != nil {
		t.Fatalf("Interactive: %v", err)
	}
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if got != want {
		t.Fatalf("guest ran %q, want %q", got, want)
	}
}

func TestInteractiveZeroExit(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	srv.SetExecHandler(func(string, io.Reader, io.Writer, io.Writer) int { return 0 })

	code, err := c.Interactive(context.Background(), "true", nil)
	if err != nil || code != 0 {
		t.Fatalf("Interactive = (%d, %v), want (0, nil)", code, err)
	}
}

// TestInteractiveStdioThroughFile exercises the tty != nil branch without a
// terminal: stdin is drained to EOF and stdout lands where the caller pointed
// it. A regular file stands in for the terminal, which also proves Wait does
// not hang waiting on the stdin copier.
func TestInteractiveStdioThroughFile(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	srv.SetExecHandler(func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		in, _ := io.ReadAll(stdin)
		_, _ = io.WriteString(stdout, "saw:"+string(in))
		return 0
	})

	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, "from the host"); err != nil {
		t.Fatalf("seed stdin: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	code, err := c.Interactive(context.Background(), "cat", f)
	if err != nil || code != 0 {
		t.Fatalf("Interactive = (%d, %v)", code, err)
	}

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(out), "saw:from the host") {
		t.Fatalf("stdio did not round trip: %q", out)
	}
}

// TestInteractiveEmptyCommandRequestsShell pins the bare-login path: an empty
// command must become a shell request, not an exec of "".
func TestInteractiveEmptyCommandRequestsShell(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	seen := make(chan string, 1)
	srv.SetExecHandler(func(cmd string, _ io.Reader, _, _ io.Writer) int {
		seen <- cmd
		return 3
	})

	code, err := c.Interactive(context.Background(), "", nil)
	if err != nil || code != 3 {
		t.Fatalf("Interactive = (%d, %v), want (3, nil)", code, err)
	}
	select {
	case cmd := <-seen:
		if cmd != "" {
			t.Fatalf("shell request carried a command %q", cmd)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no shell request reached the guest")
	}
}

func TestInteractiveReportsTransportFailure(t *testing.T) {
	t.Parallel()

	_, c := dialTestServer(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	code, err := c.Interactive(context.Background(), "true", nil)
	if err == nil {
		t.Fatal("Interactive on a closed client succeeded")
	}
	// 255 is ssh(1)'s "the session failed", distinguishable from any status a
	// remote command could plausibly return.
	if code != transportExitCode {
		t.Fatalf("exit code = %d, want %d", code, transportExitCode)
	}
}

func TestInteractiveCancellation(t *testing.T) {
	t.Parallel()

	srv, c := dialTestServer(t)
	running := make(chan struct{})
	release := make(chan struct{})
	srv.SetExecHandler(func(string, io.Reader, io.Writer, io.Writer) int {
		close(running)
		<-release
		return 0
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Interactive(ctx, "sleep forever", nil)
		done <- err
	}()

	<-running
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled session returned success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelling the context did not end the session")
	}
}
