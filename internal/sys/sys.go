// Package sys holds the two seams that make the rest of Plastic Turtle
// testable: a Clock and a process Runner.
//
// Rule for every other package: time.Now, time.Sleep, time.Tick and
// exec.Command may appear in this package and nowhere else. A supervisor that
// calls time.Sleep directly cannot have its 3-second teardown debounce tested
// in under 3 seconds, and a test that takes 3 seconds does not get run.
package sys

import (
	"context"
	"os"
	"time"
)

// Clock abstracts wall-clock time and timers.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker mirrors *time.Ticker behind an interface so fakes can drive it.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Runner abstracts subprocess execution.
//
// Run is for short commands whose output is captured (tart clone, tart ip,
// tart list). Start is for long-lived children whose handle must be retained
// (tart run).
type Runner interface {
	// Run executes name with args and returns its combined stdout. A non-zero
	// exit yields an error whose message includes captured stderr.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)

	// Start launches name with args and returns immediately.
	Start(ctx context.Context, name string, args ...string) (Process, error)
}

// Process is a running child started by Runner.Start.
type Process interface {
	// Pid reports the child's process ID.
	Pid() int

	// Wait blocks until the child exits. It is safe to call from one goroutine
	// only; callers that need multiple waiters should fan out from a single
	// Wait.
	Wait() error

	// Signal delivers sig to the child.
	Signal(sig os.Signal) error
}
