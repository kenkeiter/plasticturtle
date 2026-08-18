// Package ports resolves host-port conflicts and renders the pt ports table.
//
// Conflict resolution happens in the interactive pt shell, never in the
// supervisor: the supervisor is detached and has no terminal to prompt on. The
// shell binds each port to prove it is free, hands the resolved list to the
// supervisor, and releases the probes just before spawning it.
package ports

import (
	"context"
	"io"
	"net"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

// Resolved is one forward after conflict resolution.
type Resolved struct {
	VMPort   int
	HostPort int

	// OriginalHostPort is nonzero only when the configured port was taken.
	OriginalHostPort int
}

// Remapped reports whether the configured port had to be changed.
func (r Resolved) Remapped() bool { return r.OriginalHostPort != 0 }

// Prompter is how Negotiate talks to the user. When Interactive is false — no
// TTY, as in a script or a CI run — Negotiate silently takes the automatic
// port and reports it on Out rather than blocking forever on a read.
type Prompter struct {
	In          io.Reader
	Out         io.Writer
	Interactive bool
}

// Probes are the listeners Negotiate holds to reserve resolved ports. The
// caller must Close them immediately before spawning the supervisor, which
// then rebinds. The gap is a race, but a two-instruction one; the supervisor
// retries once if it loses.
type Probes []net.Listener

// Close releases every held probe.
func (p Probes) Close() error { panic("TODO(wave2): ports.Probes.Close") }

// Negotiate binds each configured host port on loopback. For each port already
// in use it proposes a free alternative — hostPort+ptcfg.PortRemapOffset if
// that is free, otherwise a kernel-assigned ephemeral port — and prompts the
// user, accepting the proposal on a bare Enter and re-prompting if the typed
// port is also taken.
//
// The resolved list is recorded in instance.json for pt ports to display. It
// is never written back to .plasticturtle: that would change the file's bytes
// and invalidate its trust hash, turning a port collision into a security
// prompt.
func Negotiate(ctx context.Context, want []config.ResolvedPort, p Prompter) ([]Resolved, Probes, error) {
	panic("TODO(wave2): ports.Negotiate")
}

// Bind binds a single loopback host port, for the supervisor's rebind.
func Bind(hostPort int) (net.Listener, error) { panic("TODO(wave2): ports.Bind") }

// FreePort returns a free loopback port, preferring prefer when it is
// available.
func FreePort(prefer int) (int, error) { panic("TODO(wave2): ports.FreePort") }

// Status is a forward's live state, as shown by pt ports.
type Status string

const (
	// StatusForwarding means the instance is running and its supervisor is
	// beating; the listener is up.
	StatusForwarding Status = "forwarding"

	// StatusInactive means the project has no running instance.
	StatusInactive Status = "inactive"

	// StatusStale means an instance record exists but its supervisor has
	// stopped beating — the forward is not to be trusted.
	StatusStale Status = "stale"
)

// Row is one line of the pt ports table.
type Row struct {
	// ProjectPath is set only in --global mode, where rows are grouped by
	// project.
	ProjectPath string

	VMPort           int
	HostPort         int
	OriginalHostPort int
	Status           Status

	// Conflict names another project forwarding the same host port. Only
	// --global can detect this, since it is the only mode that sees every
	// project at once.
	Conflict string
}

// Rows builds the table for a single project from its instance record. A nil
// instance yields inactive rows from the config alone.
func Rows(cfg *config.Resolved, inst *state.Instance, healthy bool) []Row {
	panic("TODO(wave2): ports.Rows")
}

// GlobalRows builds the table across every project with a live supervisor,
// annotating host ports claimed by more than one project.
func GlobalRows(ctx context.Context, s *state.Store) ([]Row, error) {
	panic("TODO(wave2): ports.GlobalRows")
}

// Render writes rows as an aligned table, or as JSON when jsonOut is set.
func Render(w io.Writer, rows []Row, global, jsonOut bool) error {
	panic("TODO(wave2): ports.Render")
}
