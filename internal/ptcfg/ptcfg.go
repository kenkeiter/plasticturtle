// Package ptcfg holds the tunable constants shared across Plastic Turtle.
//
// Every timeout, poll interval and debounce named in the design document lives
// here. No other package may hard-code a duration: a literal in the middle of
// the supervisor's watch loop is a timing bug waiting to be discovered by a
// user, not by a test.
package ptcfg

import "time"

// Boot path.
const (
	// BootTimeout bounds the whole clone -> run -> SSH-reachable sequence.
	BootTimeout = 120 * time.Second

	// SSHRetryInitial and SSHRetryMax bound the exponential backoff used while
	// waiting for sshd inside a freshly booted guest. Backoff applies only on
	// the boot path; an established session that drops is reported, not retried.
	SSHRetryInitial = 250 * time.Millisecond
	SSHRetryMax     = 2 * time.Second

	// IPPollInterval is how often `tart ip` is polled while the guest acquires
	// a DHCP lease.
	IPPollInterval = 500 * time.Millisecond

	// TCPProbeTimeout bounds a single dial attempt against the guest's :22.
	TCPProbeTimeout = 2 * time.Second
)

// Supervisor loops.
const (
	// HeartbeatInterval is how often the supervisor touches its heartbeat file.
	HeartbeatInterval = 5 * time.Second

	// HeartbeatStaleAfter is the age past which readers treat a supervisor as
	// unhealthy even if its PID is still alive (three missed beats).
	HeartbeatStaleAfter = 15 * time.Second

	// SessionPollInterval is how often the supervisor rescans the session dir.
	SessionPollInterval = 2 * time.Second

	// SessionEmptyDebounce is how long the session set must stay empty before
	// teardown begins. It exists so that `exit && pt shell` re-entry does not
	// destroy the VM out from under the user.
	SessionEmptyDebounce = 3 * time.Second

	// GracefulStopTimeout bounds `tart stop` before escalating to --force.
	GracefulStopTimeout = 30 * time.Second
)

// Attach path.
const (
	// CreatingPollInterval is how often a second `pt shell` re-reads
	// instance.json while waiting for state to reach running.
	CreatingPollInterval = 250 * time.Millisecond

	// StoppingWaitTimeout bounds waiting for a stopping instance to reach dead
	// before a re-entering `pt shell` creates a fresh one.
	StoppingWaitTimeout = 45 * time.Second
)

// Locking.
const (
	// LockTimeout bounds acquiring a project's flock. Hold times are meant to
	// be sub-millisecond; anything approaching this indicates a stuck process.
	LockTimeout = 10 * time.Second

	// LockRetryInterval is the flock polling interval.
	LockRetryInterval = 20 * time.Millisecond
)

// Shell plugin.
const (
	// CheckTrustBudget is the wall-clock budget for `pt _check-trust`. The zsh
	// chpwd hook runs it on every directory change, so exceeding this is a
	// user-visible regression. Enforced by a benchmark, not at runtime.
	CheckTrustBudget = 10 * time.Millisecond
)

// PortRemapOffset is the preferred offset applied when a configured host port
// is already bound: try hostPort+10000 first, then fall back to an ephemeral
// port from the kernel.
const PortRemapOffset = 10000
