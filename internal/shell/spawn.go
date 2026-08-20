package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kenkeiter/plasticturtle/internal/state"
)

// The supervisor's log names host paths and forwarded ports, so it is as
// private as the rest of the state tree.
const (
	logDirPerm os.FileMode = 0o700
	logPerm    os.FileMode = 0o600
)

// realSpawner starts the detached supervisor with os/exec. It is the one place
// in this package that forks.
type realSpawner struct{}

func (realSpawner) Spawn(ctx context.Context, exe string, args []string, stdinData []byte, logPath string) (int, uint64, error) {
	if exe == "" {
		return 0, 0, errors.New("shell: no supervisor executable")
	}
	if logPath == "" {
		return 0, 0, errors.New("shell: no supervisor log path")
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), logDirPerm); err != nil {
		return 0, 0, fmt.Errorf("shell: create log directory: %w", err)
	}

	// O_TRUNC, per item 8 of the implementation plan: one instance, one log
	// lifetime. Nothing else truncates this file — the supervisor only appends
	// to the descriptor it inherits — so appending here would accumulate every
	// boot the project has ever had into a file nobody ever rotates.
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, logPerm)
	if err != nil {
		return 0, 0, fmt.Errorf("shell: open %s: %w", logPath, err)
	}
	// The child holds its own duplicate of this descriptor, so closing the
	// parent's copy here does not shorten the log's life.
	defer func() { _ = log.Close() }()

	// exec.Command and not exec.CommandContext: the supervisor must outlive
	// this process by design, and CommandContext would kill it the moment
	// plasticturtle shell's context was cancelled — that is, the moment the user's session
	// ended, which is exactly when the supervisor still has a VM to tear down.
	cmd := exec.Command(exe, args...)
	cmd.Stdin = bytes.NewReader(stdinData)
	cmd.Stdout, cmd.Stderr = log, log

	// Setsid puts the child in its own session with no controlling terminal.
	// Without it the supervisor would share plasticturtle shell's process group and take
	// the ^C the user aimed at their remote shell, and would be sent SIGHUP
	// when the terminal closed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("shell: start supervisor: %w", err)
	}
	pid := cmd.Process.Pid

	// Read the identity before reaping: once the child is waited for, the
	// kernel forgets its start time, and a PID alone cannot survive PID reuse.
	start, err := state.ProcStart(pid)
	if err != nil {
		// The child was spawned; refusing to report it would strand a running
		// supervisor with no record naming it. A zero start is documented as
		// unverifiable rather than dead (see state.Alive), so the record
		// degrades to an existence check until the supervisor's own claim
		// overwrites both fields from inside the process that owns them.
		start = 0
	}

	// Reap in the background. Nothing waits on the supervisor's status — it
	// reports through its log — but leaving it unwaited would make a supervisor
	// that dies early a zombie for as long as this shell session lasts, which
	// could be hours.
	go func() { _ = cmd.Wait() }()

	return pid, start, nil
}
