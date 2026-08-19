package shell

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/supervisor"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
	"github.com/kenkeiter/plasticturtle/internal/trust"
)

// testCreds are passed explicitly so that a PT_SSH_USER in the developer's
// environment cannot break the suite.
var testCreds = sshx.Credentials{User: "admin", Password: "admin"}

// baseImage is the seed image the fake tart client is created with.
const baseImage = "base"

// testConfig is the project file every test starts from. The host port is high
// and unusual to keep negotiation from prompting on a developer's machine; if
// it is taken anyway, Negotiate silently remaps (there is no TTY) and the
// assertions below deliberately do not depend on the number.
const testConfig = `version: 1
image: base
ports:
  - vm_port: 3000
    host_port: 34567
`

// safeBuffer is an io.Writer a test goroutine may read while the code under
// test is still writing to it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// spawnCall is one recorded Spawner invocation.
type spawnCall struct {
	exe   string
	args  []string
	stdin []byte
	log   string
}

// fakeSpawner is the seam that keeps every test in this file from forking. Its
// hook stands in for the supervisor: whatever a real one would have written to
// instance.json, the hook writes synchronously instead.
type fakeSpawner struct {
	mu    sync.Mutex
	calls []spawnCall
	pid   int
	start uint64
	err   error
	hook  func(spawnCall)
}

func (s *fakeSpawner) Spawn(ctx context.Context, exe string, args []string, stdinData []byte, logPath string) (int, uint64, error) {
	call := spawnCall{
		exe:   exe,
		args:  append([]string(nil), args...),
		stdin: append([]byte(nil), stdinData...),
		log:   logPath,
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	hook, err, pid, start := s.hook, s.err, s.pid, s.start
	s.mu.Unlock()

	if err != nil {
		return 0, 0, err
	}
	if hook != nil {
		hook(call)
	}
	return pid, start, nil
}

func (s *fakeSpawner) recorded() []spawnCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]spawnCall(nil), s.calls...)
}

func (s *fakeSpawner) setHook(fn func(spawnCall)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = fn
}

// harness is one project, one state root, one in-process guest.
type harness struct {
	t *testing.T

	dir       string // canonical project directory
	projectID string
	hash      string

	store *state.Store
	trust trust.Store
	vms   *tart.Fake
	clk   *sys.FakeClock
	spawn *fakeSpawner
	guest *sshx.TestServer

	in  *bytes.Reader
	out *safeBuffer
	err *safeBuffer
}

// newHarness builds a trusted project with an in-process SSH guest reachable at
// the address a running instance record will advertise.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := newBareHarness(t, testConfig)
	h.allow(h.hash)
	return h
}

// newBareHarness builds the project without trusting it, for the tests whose
// subject is the trust check itself.
func newBareHarness(t *testing.T, cfgYAML string) *harness {
	t.Helper()

	projectDir := t.TempDir()
	if cfgYAML != "" {
		if err := os.WriteFile(filepath.Join(projectDir, config.FileName), []byte(cfgYAML), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	// The canonical form is what config.Find hands the rest of the system, and
	// on macOS a temp dir differs from it (/var is a symlink to /private/var).
	// Keying trust or state on the uncanonical path would silently miss.
	dir := projectDir
	if cfgYAML != "" {
		found, err := config.Find(projectDir)
		if err != nil {
			t.Fatalf("find project: %v", err)
		}
		dir = found
	}

	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	tr, err := trust.Open(store.TrustPath())
	if err != nil {
		t.Fatalf("open trust: %v", err)
	}

	guest, err := sshx.NewTestServer(testCreds)
	if err != nil {
		t.Fatalf("start guest: %v", err)
	}
	t.Cleanup(func() { _ = guest.Close() })
	usePort(t, guest.Addr())

	pid, start, err := state.Self()
	if err != nil {
		t.Fatalf("self: %v", err)
	}

	h := &harness{
		t:         t,
		dir:       dir,
		projectID: state.ProjectID(dir),
		hash:      config.HashBytes([]byte(cfgYAML)),
		store:     store,
		trust:     tr,
		vms:       tart.NewFake(baseImage),
		// Started at the real now: state's garbage collector compares
		// CreatedAt against the wall clock, so a fake clock parked in 1970
		// would make every fresh record look abandoned.
		clk:   sys.NewFakeClock(time.Now()),
		spawn: &fakeSpawner{pid: pid, start: start},
		guest: guest,
		in:    bytes.NewReader(nil),
		out:   &safeBuffer{},
		err:   &safeBuffer{},
	}
	h.spawn.setHook(h.supervisorBoots)
	return h
}

// usePort points this package's guest ssh port at the in-process server for the
// duration of the test.
func usePort(t *testing.T, addr string) {
	t.Helper()
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port %q: %v", portText, err)
	}
	previous := sshPort
	sshPort = port
	t.Cleanup(func() { sshPort = previous })
}

func (h *harness) allow(hash string) {
	h.t.Helper()
	if err := h.trust.Allow(h.dir, hash, nil, time.Now()); err != nil {
		h.t.Fatalf("allow: %v", err)
	}
}

func (h *harness) opts() Opts {
	return Opts{Path: h.dir, In: h.in, Out: h.out, Err: h.err, SelfPath: "/usr/local/bin/pt"}
}

func (h *harness) deps() Deps {
	return Deps{
		Tart:  h.vms,
		Store: h.store,
		Trust: h.trust,
		Clock: h.clk,
		Creds: testCreds,
		Spawn: h.spawn,
	}
}

// run executes pt shell to completion on the calling goroutine.
func (h *harness) run() (int, error) {
	h.t.Helper()
	return Run(context.Background(), h.opts(), h.deps())
}

// result is the outcome of a Run started in the background.
type result struct {
	code int
	err  error
}

// start runs pt shell on its own goroutine, for the tests that have to change
// the world underneath it while it waits.
func (h *harness) start() <-chan result {
	done := make(chan result, 1)
	go func() {
		code, err := Run(context.Background(), h.opts(), h.deps())
		done <- result{code: code, err: err}
	}()
	return done
}

// await collects a backgrounded run, failing rather than hanging the suite.
func (h *harness) await(done <-chan result) result {
	h.t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(30 * time.Second):
		h.t.Fatal("pt shell did not finish")
		return result{}
	}
}

// supervisorBoots is the default fake supervisor: it publishes the running
// state the spawning shell is about to wait for.
//
// It deliberately leaves supervisorPid unset, which is the ordering in which a
// real supervisor has not yet claimed the record — so every create-path test
// also asserts that the shell filled the field in itself (plan item 15).
func (h *harness) supervisorBoots(call spawnCall) {
	h.t.Helper()
	params := h.parseParams(call)
	h.writeInstance(&state.Instance{
		InstanceName: params.InstanceName,
		ProjectPath:  params.Config.ProjectPath,
		ConfigHash:   params.ConfigHash,
		State:        state.StateRunning,
		VMIP:         "127.0.0.1",
		CreatedAt:    h.clk.Now(),
	})
}

// parseParams decodes the supervisor parameters off the recorded stdin bytes,
// which is the round trip the real supervisor performs.
func (h *harness) parseParams(call spawnCall) *supervisor.Params {
	h.t.Helper()
	p, err := supervisor.ParseParams(bytes.NewReader(call.stdin))
	if err != nil {
		h.t.Fatalf("parse supervisor params: %v", err)
	}
	return p
}

func (h *harness) writeInstance(inst *state.Instance) {
	h.t.Helper()
	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()
	if err := h.store.WriteInstance(h.projectID, inst); err != nil {
		h.t.Fatalf("write instance: %v", err)
	}
}

func (h *harness) removeInstance() {
	h.t.Helper()
	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()
	if err := h.store.RemoveProject(h.projectID); err != nil {
		h.t.Fatalf("remove project: %v", err)
	}
}

func (h *harness) instance() *state.Instance {
	h.t.Helper()
	inst, err := h.store.ReadInstance(h.projectID)
	if err != nil {
		h.t.Fatalf("read instance: %v", err)
	}
	return inst
}

func (h *harness) sessionCount() int {
	h.t.Helper()
	sessions, err := h.store.ListSessions(h.projectID)
	if err != nil {
		h.t.Fatalf("list sessions: %v", err)
	}
	return len(sessions)
}

// instanceName returns a well-formed VM name for this project. Garbage
// collection refuses to delete anything that does not match the pattern, so a
// hand-written name would make the recovery tests assert nothing.
func (h *harness) instanceName() string {
	h.t.Helper()
	name, err := state.NewInstanceName(h.projectID)
	if err != nil {
		h.t.Fatalf("instance name: %v", err)
	}
	return name
}

// liveSupervisor is an identity that state.Alive accepts: this test process.
func (h *harness) liveSupervisor() (int, uint64) {
	h.t.Helper()
	pid, start, err := state.Self()
	if err != nil {
		h.t.Fatalf("self: %v", err)
	}
	return pid, start
}

// deadSupervisor is an identity that state.Alive rejects: a PID that exists
// paired with a start time that cannot be its own. That is exactly the PID-reuse
// case the liveness check was built for, and it is deterministic — unlike
// guessing a PID that happens to be unused.
func (h *harness) deadSupervisor() (int, uint64) {
	return os.Getpid(), 1
}

// runningInstance publishes a healthy instance record pointing at the guest.
func (h *harness) runningInstance(configHash string) *state.Instance {
	h.t.Helper()
	pid, start := h.liveSupervisor()
	inst := &state.Instance{
		InstanceName:    h.instanceName(),
		ProjectPath:     h.dir,
		ConfigHash:      configHash,
		State:           state.StateRunning,
		SupervisorPID:   pid,
		SupervisorStart: start,
		VMIP:            "127.0.0.1",
		CreatedAt:       h.clk.Now(),
	}
	h.writeInstance(inst)
	return inst
}
