// Package state owns ~/.local/state/plasticturtle: the instance and session
// records, the per-project lock, and garbage collection.
//
// The no-daemon design rests entirely on this package. Any pt invocation must
// be able to reconstruct the world from these files alone, which means two
// things hold everywhere below: every record that names a PID also records
// that process's start time (see proc.go), and every mutation happens under
// the project's exclusive flock.
package state

import (
	"context"
	"regexp"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// InstanceNamePattern matches VM names this tool owns. GC deletes orphaned VMs
// by name, so this pattern is a safety boundary: a VM that does not match is
// somebody else's and is never touched.
var InstanceNamePattern = regexp.MustCompile(`^pt-[0-9a-f]{16}-[0-9a-f]{8}$`)

// InstanceState is the lifecycle state recorded in instance.json.
type InstanceState string

const (
	StateCreating InstanceState = "creating"
	StateRunning  InstanceState = "running"
	StateStopping InstanceState = "stopping"
	StateDead     InstanceState = "dead"
)

// Instance is the current instance record for a project.
type Instance struct {
	InstanceName string        `json:"instanceName"`
	ProjectPath  string        `json:"projectPath"`
	ConfigHash   string        `json:"configHash"`
	State        InstanceState `json:"state"`

	// SupervisorPID and SupervisorStart together identify the supervisor
	// process. The start time guards against PID reuse after a reboot; see
	// Alive.
	SupervisorPID   int    `json:"supervisorPid"`
	SupervisorStart uint64 `json:"supervisorStart"`

	VMIP      string    `json:"vmIp,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	Ports     []PortMap `json:"ports"`
}

// PortMap is one live forward, reflecting any runtime remapping. Remaps live
// only for the instance's lifetime and are never written back to
// .plasticturtle, which would invalidate the config's trust hash.
type PortMap struct {
	VMPort   int `json:"vmPort"`
	HostPort int `json:"hostPort"`

	// OriginalHostPort is set only when the configured port was taken and pt
	// remapped it, so pt ports can render "remapped from N".
	OriginalHostPort int `json:"originalHostPort,omitempty"`
}

// Remapped reports whether this forward differs from what the config asked for.
func (p PortMap) Remapped() bool { return p.OriginalHostPort != 0 }

// Session is one live pt shell attached to an instance.
type Session struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	ProcStart uint64    `json:"procStart"`
	StartedAt time.Time `json:"startedAt"`
	TTY       string    `json:"tty,omitempty"`
}

// Store is the on-disk state root.
type Store struct {
	// Root is the state directory, normally ~/.local/state/plasticturtle.
	Root string
}

// DefaultRoot returns ~/.local/state/plasticturtle, honoring
// XDG_STATE_HOME when set.
func DefaultRoot() (string, error) { panic("TODO(wave1): state.DefaultRoot") }

// Open returns a Store rooted at root, creating it if needed.
func Open(root string) (*Store, error) { panic("TODO(wave1): state.Open") }

// ProjectID is sha256 of the canonical project path, hex, first 16 chars. It
// keys state on disk and names VM clones.
func ProjectID(canonicalPath string) string { panic("TODO(wave1): state.ProjectID") }

// NewInstanceName returns pt-<projectID>-<8 hex chars of randomness>.
func NewInstanceName(projectID string) (string, error) {
	panic("TODO(wave1): state.NewInstanceName")
}

// Paths within the state root. These are the only place layout is encoded.
func (s *Store) TrustPath() string                     { panic("TODO(wave1)") }
func (s *Store) ProjectDir(projectID string) string    { panic("TODO(wave1)") }
func (s *Store) InstancePath(projectID string) string  { panic("TODO(wave1)") }
func (s *Store) SessionsDir(projectID string) string   { panic("TODO(wave1)") }
func (s *Store) LockPath(projectID string) string      { panic("TODO(wave1)") }
func (s *Store) HeartbeatPath(projectID string) string { panic("TODO(wave1)") }
func (s *Store) LogPath(projectID string) string       { panic("TODO(wave1)") }

// Lock is a held flock on a project's lock file.
type Lock struct{ panicPlaceholder struct{} }

// Unlock releases the lock. It is safe to call more than once.
func (l *Lock) Unlock() error { panic("TODO(wave1)") }

// Lock takes the project's exclusive lock, blocking up to ptcfg.LockTimeout.
//
// Hold times must be short. Never hold this across a VM boot, an SSH dial, or
// a user prompt: write state, release, then wait.
func (s *Store) Lock(projectID string) (*Lock, error) { panic("TODO(wave1)") }

// RLock takes the project's shared lock, for read-only status commands.
func (s *Store) RLock(projectID string) (*Lock, error) { panic("TODO(wave1)") }

// ReadInstance returns the project's instance record, or (nil, nil) if there
// is none. Callers must hold at least a shared lock.
func (s *Store) ReadInstance(projectID string) (*Instance, error) { panic("TODO(wave1)") }

// WriteInstance atomically replaces the instance record. Callers must hold the
// exclusive lock.
func (s *Store) WriteInstance(projectID string, inst *Instance) error { panic("TODO(wave1)") }

// RemoveProject deletes a project's entire state directory. Callers must hold
// the exclusive lock.
func (s *Store) RemoveProject(projectID string) error { panic("TODO(wave1)") }

// AddSession registers a session. Callers must hold the exclusive lock.
func (s *Store) AddSession(projectID string, sess *Session) error { panic("TODO(wave1)") }

// RemoveSession deregisters a session by ID. Removing an already-removed
// session is not an error: teardown paths run more than once.
func (s *Store) RemoveSession(projectID, sessionID string) error { panic("TODO(wave1)") }

// ListSessions returns every session record on disk, including stale ones.
func (s *Store) ListSessions(projectID string) ([]*Session, error) { panic("TODO(wave1)") }

// LiveSessions returns only sessions whose process is still alive, deleting
// the records of those that are not. Callers must hold the exclusive lock.
//
// This is the predicate the supervisor's teardown decision hangs on.
func (s *Store) LiveSessions(projectID string) ([]*Session, error) { panic("TODO(wave1)") }

// Heartbeat touches the project's heartbeat file.
func (s *Store) Heartbeat(projectID string) error { panic("TODO(wave1)") }

// HeartbeatAge reports how long ago the supervisor last beat. Readers combine
// this with Alive to distinguish a healthy supervisor from a wedged one.
func (s *Store) HeartbeatAge(projectID string, now time.Time) (time.Duration, error) {
	panic("TODO(wave1)")
}

// ListProjectIDs returns every project with a state directory.
func (s *Store) ListProjectIDs() ([]string, error) { panic("TODO(wave1)") }

// GC reclaims everything the design document calls stale (spec section 10):
//
//   - session records whose process is gone;
//   - projects whose supervisor is dead but whose state is not dead — the VM
//     is force-stopped and deleted, then the state directory is removed;
//   - VMs named like ours with no corresponding state directory.
//
// It only ever deletes VMs matching InstanceNamePattern. GC takes each
// project's lock itself and skips projects it cannot lock promptly, so a
// status command never blocks behind a busy project.
func (s *Store) GC(ctx context.Context, tc tart.Client) error { panic("TODO(wave1)") }

// GCProject runs GC for a single project. Callers must hold its exclusive lock.
func (s *Store) GCProject(ctx context.Context, tc tart.Client, projectID string) error {
	panic("TODO(wave1)")
}
