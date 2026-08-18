// Package shell implements pt shell: the only command most users run.
//
// It is the interactive half of the create path. Everything that needs a
// terminal happens here — trust errors, port-conflict prompts, the boot
// spinner — because the supervisor it spawns has no terminal at all.
package shell

import (
	"context"
	"io"
	"os"

	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
	"github.com/kenkeiter/plasticturtle/internal/trust"
)

// Opts configures one pt shell invocation.
type Opts struct {
	// Path is where to start searching upward for .plasticturtle; empty means
	// the working directory.
	Path    string
	Verbose bool

	In       io.Reader
	Out      io.Writer
	Err      io.Writer
	TTY      *os.File // nil when not attached to a terminal
	SelfPath string   // path to the pt binary, for spawning the supervisor
}

// Spawner starts the detached supervisor. It exists as a seam so the create
// path can be tested without actually forking.
type Spawner interface {
	// Spawn starts exe with args in a new session (setsid), with stdout and
	// stderr redirected to logPath and stdin fed from stdinData, and returns
	// the child's identity for the instance record.
	//
	// The child must survive the parent: pt shell exits when the user's
	// session ends, and the supervisor outlives it by design.
	Spawn(ctx context.Context, exe string, args []string, stdinData []byte, logPath string) (pid int, procStart uint64, err error)
}

// RealSpawner returns a Spawner backed by os/exec with Setsid set.
func RealSpawner() Spawner { return realSpawner{} }

// Deps are the injected collaborators.
type Deps struct {
	Tart  tart.Client
	Store *state.Store
	Trust trust.Store
	Clock sys.Clock
	Creds sshx.Credentials
	Spawn Spawner
}

// Run enters the project's VM, creating it if necessary, and returns the exit
// status of the user's remote shell.
//
// The flow, with lock discipline called out because it is the part that is
// easy to get wrong:
//
//   - resolve the project and verify its config is allowed at its current
//     bytes; a mismatch is a hard error, never a prompt;
//   - take the project lock, garbage-collect stale records, and decide whether
//     an instance is live;
//   - create path: validate mount sources, negotiate host ports (prompting the
//     user), write the creating record, spawn the supervisor, release the lock
//     — the lock is never held across the prompt or the boot;
//   - attach path: release the lock, then poll for running with a spinner,
//     bounded by ptcfg.BootTimeout;
//   - register this session, run the interactive SSH session, and deregister
//     under the lock on the way out, including on signal.
//
// If a live instance's snapshotted config differs from the currently allowed
// one, Run attaches anyway and says so: the running VM keeps the mounts and
// image it booted with, and the new config takes effect once every shell has
// exited.
//
// Return contract: a nil error means exitCode is the remote shell's own status
// and nothing else went wrong. A non-nil error is pt's failure, not the
// guest's, and is returned rather than printed — cobra prints what RunE
// returns, and printing here too would show the user everything twice. The
// accompanying code is 1 for pt's own failures and 255 (ssh(1)'s convention)
// when the session itself could not be carried, so a script can tell "the VM
// said no" from "we never got in".
func Run(ctx context.Context, o Opts, d Deps) (exitCode int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := newRunner(o, d)
	if err != nil {
		return exitFailure, err
	}
	return r.run(ctx)
}
