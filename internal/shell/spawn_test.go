package shell

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kenkeiter/plasticturtle/internal/state"
)

// RealSpawner is the one place this package forks, so it is the one test that
// starts a real child. It uses /bin/sh rather than the pt binary — the subject
// is the plumbing (setsid, stdin, the truncated log), not the supervisor.
func TestRealSpawnerDetachesTruncatesAndFeedsStdin(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "supervisor.log")
	stale := []byte("output from a previous instance that must not survive\n")
	if err := os.WriteFile(logPath, stale, 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	// cat drains the parameters onto the log and then the shell lingers, so the
	// child is still alive when its identity is checked.
	pid, procStart, err := RealSpawner().Spawn(context.Background(),
		"/bin/sh", []string{"-c", "cat; sleep 30"}, []byte("params\n"), logPath)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	if pid <= 0 {
		t.Fatalf("pid = %d, want a real child", pid)
	}
	if procStart == 0 {
		t.Error("start time = 0; the record cannot survive PID reuse without it")
	}
	if !state.Alive(pid, procStart) {
		t.Errorf("state.Alive(%d, %d) = false, want the identity the record will carry", pid, procStart)
	}

	// Truncation happens in the parent, before the child can write, so it is
	// already observable: nothing here waits on the child.
	if fi, err := os.Stat(logPath); err != nil {
		t.Fatalf("stat log: %v", err)
	} else if fi.Size() >= int64(len(stale)) {
		t.Errorf("log is %d bytes, want it truncated to below %d", fi.Size(), len(stale))
	}

	// Setsid makes the child a session leader, which is what keeps it from
	// taking the ^C aimed at the user's remote shell or the SIGHUP that follows
	// the terminal closing.
	if sid, err := unix.Getsid(pid); err != nil {
		t.Errorf("getsid: %v", err)
	} else if sid != pid {
		t.Errorf("session id = %d, want the child's own pid %d", sid, pid)
	}

	// The only real-time wait in the suite, and it is on an OS process rather
	// than on any of pt's own timing: the child has to be scheduled before its
	// stdout reaches the log.
	waitForLog(t, logPath, "params\n")
}

// waitForLog polls until path contains want, failing rather than hanging.
func waitForLog(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read log: %v", err)
		}
		if string(b) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want %q", b, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A spawn that cannot be attempted must not be reported as a running child.
func TestRealSpawnerRejectsBadInput(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "supervisor.log")

	if _, _, err := RealSpawner().Spawn(context.Background(), "", nil, nil, logPath); err == nil {
		t.Error("spawning with no executable succeeded")
	}
	if _, _, err := RealSpawner().Spawn(context.Background(), "/bin/sh", nil, nil, ""); err == nil {
		t.Error("spawning with no log path succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := RealSpawner().Spawn(ctx, "/bin/sh", []string{"-c", "true"}, nil, logPath); err == nil {
		t.Error("spawning with a cancelled context succeeded")
	}
}
