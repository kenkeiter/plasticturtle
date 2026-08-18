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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// InstanceNamePattern matches VM names this tool owns. GC deletes orphaned VMs
// by name, so this pattern is a safety boundary: a VM that does not match is
// somebody else's and is never touched.
var InstanceNamePattern = regexp.MustCompile(`^pt-[0-9a-f]{16}-[0-9a-f]{8}$`)

// projectIDPattern is the shape ProjectID produces. NewInstanceName checks its
// argument against it so that a generated name can never fail
// InstanceNamePattern: the generator and the safety boundary must not be able
// to drift apart.
var projectIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// sessionIDPattern bounds what may become a file name under sessions/. Session
// IDs reach us from other packages and are concatenated into a path, so an
// unvalidated "../../instance" would be a write outside the project.
var sessionIDPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

// projectIDLen is the hex width of a project ID, fixed by ProjectID and relied
// on when recovering a project ID from an instance name.
const projectIDLen = 16

// File and directory names. Nothing outside the path helpers below may name a
// file in the state root.
const (
	instancesDirName = "instances"
	trustFileName    = "trust.json"
	lockFileName     = "lock"
	instanceFileName = "instance.json"
	sessionsDirName  = "sessions"
	heartbeatName    = "heartbeat"
	logFileName      = "supervisor.log"
)

// State files can name project paths and forwarded ports, so the tree is
// owner-only.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// gcLockWait is how long GC will wait for a project's lock before skipping the
// project. It is deliberately a small multiple of the flock poll interval
// rather than ptcfg.LockTimeout: a busy project is not an error condition for
// GC, and `pt list` must never block behind somebody else's `pt shell`.
const gcLockWait = 5 * ptcfg.LockRetryInterval

// vanishRetryWindow bounds how long an acquire retries a lock file that keeps
// disappearing underneath it. See acquire.
const vanishRetryWindow = 10 * ptcfg.LockRetryInterval

// ErrLockBusy reports that a lock could not be taken within its deadline.
//
// It is exported because status commands legitimately decide for themselves
// that a busy project is a skip rather than a failure — see TryRLock. Callers
// that cannot proceed without the lock should still treat it as fatal.
var ErrLockBusy = errors.New("state: lock is held by another process")

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
func DefaultRoot() (string, error) {
	// The XDG spec says a relative value is invalid and must be ignored, which
	// matters here: a relative state root would follow the user's cwd and split
	// one project's state across directories.
	if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "plasticturtle"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "plasticturtle"), nil
}

// Open returns a Store rooted at root, creating it if needed.
func Open(root string) (*Store, error) {
	if root == "" {
		d, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = d
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("state: resolve root %q: %w", root, err)
	}
	s := &Store{Root: abs}
	if err := os.MkdirAll(s.instancesDir(), dirPerm); err != nil {
		return nil, fmt.Errorf("state: create state root: %w", err)
	}
	return s, nil
}

// ProjectID is sha256 of the canonical project path, hex, first 16 chars. It
// keys state on disk and names VM clones.
func ProjectID(canonicalPath string) string {
	sum := sha256.Sum256([]byte(canonicalPath))
	return hex.EncodeToString(sum[:])[:projectIDLen]
}

// NewInstanceName returns pt-<projectID>-<8 hex chars of randomness>.
func NewInstanceName(projectID string) (string, error) {
	// A name that does not match InstanceNamePattern would create a VM that GC
	// is forbidden to ever clean up, so reject a malformed project ID here
	// rather than leaking a VM later.
	if !projectIDPattern.MatchString(projectID) {
		return "", fmt.Errorf("state: malformed project id %q", projectID)
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("state: read randomness: %w", err)
	}
	return "pt-" + projectID + "-" + hex.EncodeToString(b[:]), nil
}

// projectIDFromInstanceName recovers the project ID embedded in a VM name. The
// caller must have already matched InstanceNamePattern.
func projectIDFromInstanceName(name string) string {
	return name[len("pt-") : len("pt-")+projectIDLen]
}

// newSessionID returns an ID satisfying sessionIDPattern.
func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("state: read randomness: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// safeComponent keeps a caller's bug inside the state root. The path helpers
// return a string and cannot report an error, so an ID that is not a single
// safe path element is mapped to a distinctive, deterministic stand-in instead
// of being pasted into a path where "../.." would escape.
func safeComponent(id string) string {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`+"\x00") {
		sum := sha256.Sum256([]byte(id))
		return "invalid-" + hex.EncodeToString(sum[:])[:16]
	}
	return id
}

func (s *Store) instancesDir() string { return filepath.Join(s.Root, instancesDirName) }

// Paths within the state root. These are the only place layout is encoded.
func (s *Store) TrustPath() string { return filepath.Join(s.Root, trustFileName) }
func (s *Store) ProjectDir(projectID string) string {
	return filepath.Join(s.instancesDir(), safeComponent(projectID))
}
func (s *Store) InstancePath(projectID string) string {
	return filepath.Join(s.ProjectDir(projectID), instanceFileName)
}
func (s *Store) SessionsDir(projectID string) string {
	return filepath.Join(s.ProjectDir(projectID), sessionsDirName)
}
func (s *Store) LockPath(projectID string) string {
	return filepath.Join(s.ProjectDir(projectID), lockFileName)
}
func (s *Store) HeartbeatPath(projectID string) string {
	return filepath.Join(s.ProjectDir(projectID), heartbeatName)
}
func (s *Store) LogPath(projectID string) string {
	return filepath.Join(s.ProjectDir(projectID), logFileName)
}

func (s *Store) sessionPath(projectID, sessionID string) string {
	return filepath.Join(s.SessionsDir(projectID), safeComponent(sessionID)+".json")
}

// Lock is a held flock on a project's lock file.
type Lock struct {
	fl   *flock.Flock
	once sync.Once
	err  error
}

// Unlock releases the lock. It is safe to call more than once.
//
// Every acquisition site defers Unlock and the successful paths also release
// early (state is written, then the lock is dropped before any wait); calling
// it twice must therefore be a no-op rather than an error, and must not
// release a lock some later acquisition now holds.
func (l *Lock) Unlock() error {
	if l == nil || l.fl == nil {
		return nil
	}
	l.once.Do(func() { l.err = l.fl.Unlock() })
	return l.err
}

// Lock takes the project's exclusive lock, blocking up to ptcfg.LockTimeout.
//
// Hold times must be short. Never hold this across a VM boot, an SSH dial, or
// a user prompt: write state, release, then wait.
func (s *Store) Lock(projectID string) (*Lock, error) {
	return s.acquire(projectID, true, ptcfg.LockTimeout, true)
}

// RLock takes the project's shared lock, for read-only status commands.
func (s *Store) RLock(projectID string) (*Lock, error) {
	return s.acquire(projectID, false, ptcfg.LockTimeout, true)
}

// TryRLock takes the project's shared lock with a caller-chosen deadline,
// returning ErrLockBusy rather than waiting out ptcfg.LockTimeout.
//
// It exists for status sweeps — pt ports --global and pt list — which visit
// every project, where waiting the full ten seconds on each wedged one would
// cost N×10s and where a busy project is a skip rather than a failure.
//
// Unlike Lock and RLock it does NOT create the project directory, so a caller
// that only observes cannot resurrect state a supervisor has just removed.
// Note that pt shell's boot poller does NOT use this and therefore does not
// have that property: it takes RLock, which creates. The empty directory that
// leaves behind after a failed boot is reclaimed by the next GCProject.
//
// A missing project directory surfaces as an fs.ErrNotExist-wrapped error.
func (s *Store) TryRLock(projectID string, wait time.Duration) (*Lock, error) {
	return s.acquire(projectID, false, wait, false)
}

// acquire polls for the lock at ptcfg.LockRetryInterval until wait elapses.
// Polling rather than a blocking flock(2) is what makes the timeout possible:
// a supervisor that wedges while holding the lock must not hang every other pt
// invocation forever.
//
// create distinguishes callers that may bring a project into existence from
// those that may only observe one; see TryRLock.
func (s *Store) acquire(projectID string, exclusive bool, wait time.Duration, create bool) (*Lock, error) {
	// A creating caller retries, within its own deadline, when the directory it
	// just made is gone again before it can open the lock file inside it.
	//
	// This is not hypothetical: a supervisor's teardown removes the whole
	// project directory, and it can land between the MkdirAll and the open
	// below. The victim is some entirely unrelated pt invocation, which fails
	// with a bare "no such file or directory" naming a path it never chose. A
	// vanished directory means somebody else is finishing up — exactly the
	// condition the lock's existing wait budget is for — so it is treated like
	// contention rather than like failure. Found by racing four pt shells at
	// one project.
	if !create {
		return s.acquireOnce(projectID, exclusive, wait, create)
	}

	// Bounded separately from wait, and tightly: the window between a MkdirAll
	// and the open inside it is microseconds, so a directory that is still
	// vanishing after this long is not a race we can wait out. Using the full
	// lock timeout here would let four contending shells compound into minutes.
	deadline := time.Now().Add(vanishRetryWindow)
	for {
		lk, err := s.acquireOnce(projectID, exclusive, wait, create)
		if err == nil {
			return lk, nil
		}
		if !vanishedUnderUs(err) || !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(ptcfg.LockRetryInterval)
	}
}

// vanishedUnderUs reports whether an acquire failed because the lock file it
// was working with stopped existing mid-flight.
//
// ENOENT is the directory being removed before the open; EINVAL is the file
// being unlinked and replaced between the open and the flock, which leaves a
// descriptor the kernel will not lock. Both mean "another process is finishing
// up here", not "this lock is unobtainable".
func vanishedUnderUs(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EINVAL)
}

func (s *Store) acquireOnce(projectID string, exclusive bool, wait time.Duration, create bool) (*Lock, error) {
	dir := s.ProjectDir(projectID)
	if create {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("state: create project dir: %w", err)
		}
	} else if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("state: project dir %s: %w", dir, err)
	}
	fl := flock.New(s.LockPath(projectID), flock.SetPermissions(filePerm))

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	var ok bool
	var err error
	if exclusive {
		ok, err = fl.TryLockContext(ctx, ptcfg.LockRetryInterval)
	} else {
		ok, err = fl.TryRLockContext(ctx, ptcfg.LockRetryInterval)
	}
	if err != nil || !ok {
		// The descriptor is useless now and GC runs this path on every busy
		// project, so it has to be closed rather than left to the finalizer.
		_ = fl.Close()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("state: lock %s after %s: %w", dir, wait, ErrLockBusy)
		}
		return nil, fmt.Errorf("state: lock %s: %w", dir, err)
	}
	return &Lock{fl: fl}, nil
}

// ReadInstance returns the project's instance record, or (nil, nil) if there
// is none. Callers must hold at least a shared lock.
func (s *Store) ReadInstance(projectID string) (*Instance, error) {
	b, err := os.ReadFile(s.InstancePath(projectID))
	if err != nil {
		// Absence is the normal state of most projects most of the time; it is
		// the answer "no instance", not a failure.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: read instance record: %w", err)
	}
	var inst Instance
	if err := json.Unmarshal(b, &inst); err != nil {
		return nil, fmt.Errorf("state: parse %s: %w", s.InstancePath(projectID), err)
	}
	return &inst, nil
}

// WriteInstance atomically replaces the instance record. Callers must hold the
// exclusive lock.
func (s *Store) WriteInstance(projectID string, inst *Instance) error {
	if inst == nil {
		return errors.New("state: WriteInstance: nil instance")
	}
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode instance record: %w", err)
	}
	return atomicWrite(s.InstancePath(projectID), append(b, '\n'))
}

// RemoveProject deletes a project's entire state directory. Callers must hold
// the exclusive lock.
//
// This removes the lock file the caller is holding. That is safe only because
// the held flock lives on the open file description, not the path: a process
// that arrives afterwards creates a fresh lock file and is correctly
// unblocked. It does mean this must be the caller's *last* mutation before
// releasing — anything written after it would be written outside the mutual
// exclusion the caller believes it still has.
func (s *Store) RemoveProject(projectID string) error {
	if err := os.RemoveAll(s.ProjectDir(projectID)); err != nil {
		return fmt.Errorf("state: remove project dir: %w", err)
	}
	return nil
}

// AddSession registers a session. Callers must hold the exclusive lock.
func (s *Store) AddSession(projectID string, sess *Session) error {
	if sess == nil {
		return errors.New("state: AddSession: nil session")
	}
	if sess.ID == "" {
		id, err := newSessionID()
		if err != nil {
			return err
		}
		sess.ID = id
	}
	if !sessionIDPattern.MatchString(sess.ID) {
		return fmt.Errorf("state: malformed session id %q", sess.ID)
	}
	if err := os.MkdirAll(s.SessionsDir(projectID), dirPerm); err != nil {
		return fmt.Errorf("state: create sessions dir: %w", err)
	}
	b, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("state: encode session record: %w", err)
	}
	return atomicWrite(s.sessionPath(projectID, sess.ID), append(b, '\n'))
}

// RemoveSession deregisters a session by ID. Removing an already-removed
// session is not an error: teardown paths run more than once.
func (s *Store) RemoveSession(projectID, sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return fmt.Errorf("state: malformed session id %q", sessionID)
	}
	if err := os.Remove(s.sessionPath(projectID, sessionID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: remove session record: %w", err)
	}
	return nil
}

// ListSessions returns every session record on disk, including stale ones.
func (s *Store) ListSessions(projectID string) ([]*Session, error) {
	sessions, _, err := s.readSessions(projectID)
	return sessions, err
}

// readSessions returns the parsed session records plus the names of files that
// could not be used (unparseable, or missing an ID). LiveSessions treats those
// as stale and deletes them; ListSessions ignores them.
func (s *Store) readSessions(projectID string) (sessions []*Session, bad []string, err error) {
	dir := s.SessionsDir(projectID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("state: list sessions: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A concurrent RemoveSession won the race; not our problem.
				continue
			}
			return nil, nil, fmt.Errorf("state: read session record: %w", err)
		}
		var sess Session
		if err := json.Unmarshal(b, &sess); err != nil || sess.ID == "" {
			bad = append(bad, e.Name())
			continue
		}
		sessions = append(sessions, &sess)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, bad, nil
}

// LiveSessions returns only sessions whose process is still alive, deleting
// the records of those that are not. Callers must hold the exclusive lock.
//
// This is the predicate the supervisor's teardown decision hangs on.
func (s *Store) LiveSessions(projectID string) ([]*Session, error) {
	all, bad, err := s.readSessions(projectID)
	if err != nil {
		return nil, err
	}
	dir := s.SessionsDir(projectID)
	var errs []error

	// An unreadable record names a session nobody can ever prove is alive.
	// Leaving it would pin the VM open forever, so it is stale by definition.
	for _, name := range bad {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	live := make([]*Session, 0, len(all))
	for _, sess := range all {
		if Alive(sess.PID, sess.ProcStart) {
			live = append(live, sess)
			continue
		}
		if err := s.RemoveSession(projectID, sess.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return live, errors.Join(errs...)
}

// Heartbeat touches the project's heartbeat file.
func (s *Store) Heartbeat(projectID string) error {
	if err := os.MkdirAll(s.ProjectDir(projectID), dirPerm); err != nil {
		return fmt.Errorf("state: create project dir: %w", err)
	}
	p := s.HeartbeatPath(projectID)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("state: open heartbeat: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close heartbeat: %w", err)
	}
	// The beat is the mtime, not the contents: readers stat it, so there is
	// nothing to write and nothing to fsync. time.Now is used directly rather
	// than through a sys.Clock because the value must come from the same clock
	// the filesystem stamps and HeartbeatAge later compares against.
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		return fmt.Errorf("state: touch heartbeat: %w", err)
	}
	return nil
}

// HeartbeatAge reports how long ago the supervisor last beat. Readers combine
// this with Alive to distinguish a healthy supervisor from a wedged one.
func (s *Store) HeartbeatAge(projectID string, now time.Time) (time.Duration, error) {
	fi, err := os.Stat(s.HeartbeatPath(projectID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("state: no heartbeat for %s: %w", projectID, os.ErrNotExist)
		}
		return 0, fmt.Errorf("state: stat heartbeat: %w", err)
	}
	age := now.Sub(fi.ModTime())
	if age < 0 {
		// A beat stamped in the future (clock adjustment) is not stale.
		age = 0
	}
	return age, nil
}

// ListProjectIDs returns every project with a state directory.
func (s *Store) ListProjectIDs() ([]string, error) {
	entries, err := os.ReadDir(s.instancesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: list projects: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		// Only well-formed IDs are ours. GC iterates this list and deletes
		// VMs, so anything that could not have come from ProjectID is left
		// strictly alone.
		if e.IsDir() && projectIDPattern.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

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
func (s *Store) GC(ctx context.Context, tc tart.Client) error {
	ids, err := s.ListProjectIDs()
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		lk, err := s.acquire(id, true, gcLockWait, true)
		if err != nil {
			// Busy means somebody is actively working on this project, which
			// is exactly when there is nothing to collect. Any other lock
			// failure is still not worth failing a status command over.
			continue
		}
		gcErr := s.GCProject(ctx, tc, id)
		unlockErr := lk.Unlock()
		if gcErr != nil {
			errs = append(errs, fmt.Errorf("gc project %s: %w", id, gcErr))
		}
		if unlockErr != nil {
			errs = append(errs, fmt.Errorf("gc project %s: unlock: %w", id, unlockErr))
		}
	}

	// The orphan sweep runs after the per-project pass so it never re-examines
	// a VM that pass just deleted.
	if err := s.gcOrphanVMs(ctx, tc); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// GCProject runs GC for a single project. Callers must hold its exclusive lock.
func (s *Store) GCProject(ctx context.Context, tc tart.Client, projectID string) error {
	inst, err := s.ReadInstance(projectID)
	if err != nil {
		// A record we cannot parse names a VM we cannot identify. Deleting
		// state on a guess risks orphaning that VM under a name we no longer
		// know, so leave everything and report.
		return err
	}

	live, sessErr := s.LiveSessions(projectID)

	if inst == nil {
		// No record. Once the dead sessions are gone there is nothing here to
		// reconstruct the world from, so the directory is litter — but only if
		// nothing live is still pointing at it.
		if len(live) == 0 {
			return errors.Join(sessErr, s.RemoveProject(projectID))
		}
		return sessErr
	}

	if !s.supervisorReclaimable(inst) {
		return sessErr
	}

	// The supervisor is gone: nothing will ever move this instance forward,
	// its sessions are attached to an SSH server that is about to disappear,
	// and the VM (if any) is unmanaged. Reclaim both.
	var errs []error
	if sessErr != nil {
		errs = append(errs, sessErr)
	}
	if err := forceDeleteVM(ctx, tc, inst.InstanceName); err != nil {
		// If the VM cannot be removed, keep the record: it is the only thing
		// that still names the VM, and the next GC will try again.
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	errs = append(errs, s.RemoveProject(projectID))
	return errors.Join(errs...)
}

// supervisorReclaimable reports whether an instance record may be torn down.
//
// The plain rule is "the supervisor PID is not alive". The exception is the
// window opened by the spec's own ordering: pt shell writes the record and
// *then* spawns the supervisor, so a genuinely healthy instance can briefly
// carry no supervisor PID at all. Treating that as dead would have GC delete
// the VM of the shell that is booting it, so an un-supervised record is given
// until the boot timeout to acquire one.
func (s *Store) supervisorReclaimable(inst *Instance) bool {
	if Alive(inst.SupervisorPID, inst.SupervisorStart) {
		// A live but wedged supervisor (stale heartbeat) is deliberately left
		// alone: killing a running process's VM out from under it is worse
		// than leaving a stuck instance for the user to see in pt list.
		return false
	}
	if inst.SupervisorPID <= 0 && !inst.CreatedAt.IsZero() {
		if time.Since(inst.CreatedAt) < ptcfg.BootTimeout {
			return false
		}
	}
	return true
}

// gcOrphanVMs deletes VMs that carry our name shape but that no state
// directory claims.
//
// Correctness rests on one ordering fact: pt writes the instance record before
// anything creates the VM. So if a VM is visible in tart's list, its record —
// if it was ever going to have one — is already on disk, and an absent record
// means abandoned rather than not-yet-created.
func (s *Store) gcOrphanVMs(ctx context.Context, tc tart.Client) error {
	if tc == nil {
		return nil
	}
	vms, err := tc.List(ctx)
	if err != nil {
		return fmt.Errorf("state: list vms: %w", err)
	}
	var errs []error
	for _, vm := range vms {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The safety boundary. Everything below this line can delete a VM, so
		// nothing that is not provably ours may get past it.
		if !InstanceNamePattern.MatchString(vm.Name) {
			continue
		}
		// tart list also reports cached OCI images; those are never clones we
		// made, whatever they are called.
		if vm.Source == tart.SourceOCI {
			continue
		}
		claimed, err := s.vmIsClaimed(vm.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if claimed {
			continue
		}
		if err := forceDeleteVM(ctx, tc, vm.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// vmIsClaimed reports whether some project's instance record names this VM.
// Every uncertainty — a busy project, an unreadable record — answers "yes":
// the cost of not deleting an orphan is disk space, the cost of deleting a
// live VM is the user's session.
func (s *Store) vmIsClaimed(vmName string) (bool, error) {
	projectID := projectIDFromInstanceName(vmName)
	if _, err := os.Stat(s.ProjectDir(projectID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return true, fmt.Errorf("state: stat project dir: %w", err)
	}
	lk, err := s.acquire(projectID, false, gcLockWait, false)
	if err != nil {
		return true, nil
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := s.ReadInstance(projectID)
	if err != nil {
		return true, err
	}
	if inst == nil {
		return false, nil
	}
	// A record naming a *different* VM means this one is a leftover from an
	// earlier instance of the same project, and is just as orphaned.
	return inst.InstanceName == vmName, nil
}

// forceDeleteVM stops and deletes a VM, tolerating every "it was already gone"
// outcome. It refuses to touch a name that is not ours; every deletion path in
// this package funnels through here so that check exists exactly once.
func forceDeleteVM(ctx context.Context, tc tart.Client, name string) error {
	if tc == nil || name == "" {
		return nil
	}
	if !InstanceNamePattern.MatchString(name) {
		return fmt.Errorf("state: refusing to delete vm %q: not a plasticturtle instance", name)
	}

	// Bounded, because every caller of this holds the project's exclusive lock
	// and these are subprocesses whose duration pt does not control. An
	// unbounded `tart delete` here blocks every other pt invocation for the
	// project indefinitely; with a deadline the reclaim gives up, the lock is
	// released, and the next GC pass tries again.
	ctx, cancel := context.WithTimeout(ctx, ptcfg.ReclaimTimeout)
	defer cancel()

	// A stopped VM makes `tart stop` fail; that is the outcome we wanted, so
	// only the delete result decides success.
	_ = tc.Stop(ctx, name, true)

	if err := tc.Delete(ctx, name); err != nil && !errors.Is(err, tart.ErrNotFound) {
		return fmt.Errorf("state: delete vm %s: %w", name, err)
	}
	return nil
}

// atomicWrite replaces path with data, or leaves the previous contents intact.
//
// A reader of these records holds only a shared lock, and GC reads some of
// them under no lock at all, so a half-written instance.json must never be
// observable: the temp file goes in the same directory (rename must not cross
// filesystems), is fsynced before it is named, and the parent directory is
// fsynced afterwards so the rename itself survives a crash.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("state: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("state: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Harmless after a successful rename; the only cleanup path that
		// matters is the failing one.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: sync %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: rename onto %s: %w", path, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: open %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", dir, err)
	}
	return nil
}
