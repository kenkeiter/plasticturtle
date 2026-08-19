package sys

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type realClock struct{}

// RealClock returns a Clock backed by the time package.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

type realRunner struct{}

// RealRunner returns a Runner backed by os/exec.
func RealRunner() Runner { return realRunner{} }

func (realRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Stderr is the useful part of a tart failure; without it the caller
		// gets "exit status 1" and nothing to act on.
		return stdout.Bytes(), fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func (r realRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	return r.StartEnv(ctx, name, nil, args...)
}

func (realRunner) StartEnv(ctx context.Context, name string, env []string, args ...string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// A long-lived child's output is the supervisor's log; callers redirect by
	// setting their own process stdio before Start is reached.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		// os/exec uses the last value for a repeated key, so appending lets a
		// caller-supplied PATH or PT_* override the inherited environment.
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	return &realProcess{cmd: cmd}, nil
}

type realProcess struct{ cmd *exec.Cmd }

func (p *realProcess) Pid() int    { return p.cmd.Process.Pid }
func (p *realProcess) Wait() error { return p.cmd.Wait() }

func (p *realProcess) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("signal: process not started")
	}
	return p.cmd.Process.Signal(sig)
}
