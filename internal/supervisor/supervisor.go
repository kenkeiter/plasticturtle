// Package supervisor implements pt _supervise, the detached process that owns
// one instance for its entire life.
//
// It is not a daemon. It is spawned by the pt shell that creates an instance,
// it owns exactly that instance's tart run child and SSH tunnels, and it exits
// when the last session leaves. Nothing else in the system outlives a VM.
//
// Its parameters arrive as JSON on stdin rather than argv: mount paths and
// ports would otherwise be visible in ps output to every user on the machine.
package supervisor

import (
	"context"
	"io"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// Params is everything the supervisor needs, decoded from stdin.
//
// The config is snapshotted here at instance creation. Later edits to
// .plasticturtle — even re-allowed ones — apply only to the next instance,
// which is what makes a running VM's mounts and image immutable for its
// lifetime.
type Params struct {
	ProjectID    string           `json:"projectId"`
	InstanceName string           `json:"instanceName"`
	ConfigHash   string           `json:"configHash"`
	Config       *config.Resolved `json:"config"`
	Ports        []ports.Resolved `json:"ports"`
	StateRoot    string           `json:"stateRoot"`
}

// ParseParams decodes Params from r, validating that every required field is
// present.
func ParseParams(r io.Reader) (*Params, error) { panic("TODO(wave2): supervisor.ParseParams") }

// EncodeParams writes p as JSON, for pt shell to pipe to the child.
func EncodeParams(w io.Writer, p *Params) error { panic("TODO(wave2): supervisor.EncodeParams") }

// Deps are the supervisor's injected collaborators. Tests supply a tart.Fake,
// a sys.FakeClock and a temp state root, which is what makes the full
// lifecycle — including the 3-second teardown debounce — assertable in
// microseconds.
type Deps struct {
	Tart  tart.Client
	Store *state.Store
	Clock sys.Clock
	Creds sshx.Credentials

	// Logf writes to supervisor.log. It is the only channel the supervisor
	// has: it is detached, so anything it prints to a terminal goes nowhere.
	Logf func(format string, args ...any)
}

// Run executes the full instance lifecycle and returns when the instance is
// dead:
//
//  1. clone the image, apply resource overrides, boot with the configured
//     directory shares;
//  2. poll for an address and for sshd, bounded by ptcfg.BootTimeout;
//  3. open the SSH connection and every forward;
//  4. publish state running with the guest's address;
//  5. watch three things concurrently — the heartbeat, the session directory,
//     and the tart run child;
//  6. tear down when the session set has been empty for the debounce, or when
//     the VM dies, or on SIGTERM.
//
// A boot failure marks the instance dead and cleans up the clone before
// returning; the waiting pt shell reports it. Teardown is idempotent, because
// it can be entered from any of the three watchers at once.
func Run(ctx context.Context, p *Params, d Deps) error { panic("TODO(wave2): supervisor.Run") }
