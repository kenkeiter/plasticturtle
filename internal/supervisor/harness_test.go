package supervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
	"github.com/kenkeiter/plasticturtle/internal/trust"
)

// testCreds are the guest credentials both the fake guest and the supervisor
// under test use. They are passed explicitly so that a PT_SSH_USER in the
// developer's environment cannot break the suite.
var testCreds = sshx.Credentials{User: "admin", Password: "admin"}

// runningWaiters is the number of clock waiters that exist once an instance is
// running: the boot deadline (fired only on failure, but registered for the
// whole run), the heartbeat ticker, and the session poll.
//
// BlockUntil on this number is the suite's synchronization primitive. Reaching
// it means the boot finished, state running was published, and the session
// watcher has re-armed — which is exactly "the supervisor is idle, advance the
// clock now" with no sleeping involved.
const runningWaiters = 3

// baseImage is the seed image tart.Fake is created with.
const baseImage = "base"

// testLog captures the supervisor's only output channel.
type testLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *testLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// withPrefix returns the logged lines starting with prefix, in order.
func (l *testLog) withPrefix(prefix string) []string {
	var out []string
	for _, line := range l.all() {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

// states is the sequence of lifecycle states the supervisor wrote.
func (l *testLog) states() []string {
	var out []string
	for _, line := range l.withPrefix("state: ") {
		out = append(out, strings.TrimPrefix(line, "state: "))
	}
	return out
}

// countingTart records how many times each lifecycle method was called, which
// is how "teardown ran exactly once" is asserted without inspecting internals.
type countingTart struct {
	tart.Client
	mu          sync.Mutex
	calls       map[string]int
	lastRunOpts tart.RunOpts
}

func newCountingTart(c tart.Client) *countingTart {
	return &countingTart{Client: c, calls: map[string]int{}}
}

func (c *countingTart) note(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[method]++
}

func (c *countingTart) n(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

func (c *countingTart) Clone(ctx context.Context, image, name string) error {
	c.note("Clone")
	return c.Client.Clone(ctx, image, name)
}

func (c *countingTart) Set(ctx context.Context, name string, cpu, memoryMiB int) error {
	c.note("Set")
	return c.Client.Set(ctx, name, cpu, memoryMiB)
}

func (c *countingTart) Run(ctx context.Context, name string, opts tart.RunOpts) (sys.Process, error) {
	c.note("Run")
	c.mu.Lock()
	c.lastRunOpts = opts
	c.mu.Unlock()
	return c.Client.Run(ctx, name, opts)
}

func (c *countingTart) Stop(ctx context.Context, name string, force bool) error {
	c.note("Stop")
	if force {
		c.note("StopForce")
	}
	return c.Client.Stop(ctx, name, force)
}

func (c *countingTart) Delete(ctx context.Context, name string) error {
	c.note("Delete")
	return c.Client.Delete(ctx, name)
}

func (c *countingTart) runOpts() tart.RunOpts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRunOpts
}

// harness is one supervisor under test, wired to a fake hypervisor, a fake
// clock, a temporary state root and a real in-process SSH server.
type harness struct {
	t *testing.T

	store      *state.Store
	fake       *tart.Fake
	tc         *countingTart
	clk        *sys.FakeClock
	log        *testLog
	srv        *sshx.TestServer
	params     *Params
	deps       Deps
	projectID  string
	instance   string
	projectDir string

	done   chan error
	cancel context.CancelFunc
}

func newHarness(t *testing.T, forwards ...ports.Resolved) *harness {
	t.Helper()

	projectDir := t.TempDir()
	dataDir := t.TempDir()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	projectID := state.ProjectID(projectDir)
	instance, err := state.NewInstanceName(projectID)
	if err != nil {
		t.Fatalf("NewInstanceName: %v", err)
	}

	fake := tart.NewFake(baseImage)
	// The guest is this process: tart reports 127.0.0.1, and the SSH server the
	// supervisor dials is an in-process sshx.TestServer on loopback.
	fake.SetIP(instance, "127.0.0.1")

	srv, err := sshx.NewTestServer(testCreds)
	if err != nil {
		t.Fatalf("NewTestServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	setSSHPort(t, portOf(t, srv.Addr()))

	lg := &testLog{}
	clk := sys.NewFakeClock(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC))
	tc := newCountingTart(fake)

	h := &harness{
		t:          t,
		store:      store,
		fake:       fake,
		tc:         tc,
		clk:        clk,
		log:        lg,
		srv:        srv,
		projectID:  projectID,
		instance:   instance,
		projectDir: projectDir,
		params: &Params{
			ProjectID:    projectID,
			InstanceName: instance,
			ConfigHash:   "sha256:" + strings.Repeat("a", 64),
			StateRoot:    store.Root,
			Ports:        forwards,
			Config: &config.Resolved{
				ProjectPath: projectDir,
				Image:       baseImage,
				Mounts: []config.ResolvedMount{
					{Name: "project", HostPath: projectDir, Mode: config.ModeRW},
					{Name: "data", HostPath: dataDir, Mode: config.ModeRO},
				},
			},
		},
		deps: Deps{Tart: tc, Store: store, Clock: clk, Creds: testCreds, Logf: lg.logf},
	}

	// The supervisor refuses to boot a config the trust database does not
	// know, so the harness records the approval pt allow would have recorded.
	// A test that wants the refusal path clears it; see trust_test.go.
	ts, err := trust.Open(store.TrustPath())
	if err != nil {
		t.Fatalf("open trust: %v", err)
	}
	if err := ts.Allow(projectDir, h.params.ConfigHash, nil, time.Now()); err != nil {
		t.Fatalf("allow: %v", err)
	}
	h.deps.Trust = ts

	return h
}

// writeCreating does what pt shell does before spawning the supervisor.
func (h *harness) writeCreating() {
	h.t.Helper()
	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	err = h.store.WriteInstance(h.projectID, &state.Instance{
		InstanceName: h.instance,
		ProjectPath:  h.projectDir,
		ConfigHash:   h.params.ConfigHash,
		State:        state.StateCreating,
		CreatedAt:    h.clk.Now(),
	})
	if err != nil {
		h.t.Fatalf("write instance: %v", err)
	}
}

// start launches Run in the background.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(cancel)
	h.done = make(chan error, 1)
	go func() { h.done <- Run(ctx, h.params, h.deps) }()
}

// waitRunning blocks until the instance is running and both watchers are armed.
func (h *harness) waitRunning() {
	h.t.Helper()
	h.clk.BlockUntil(runningWaiters)
	if got := h.instanceState(); got != state.StateRunning {
		h.t.Fatalf("state after boot = %q, want running", got)
	}
}

// tick advances the clock by d and waits for the session watcher to re-arm, so
// that the next advance cannot race an unprocessed tick.
func (h *harness) tick(d time.Duration) {
	h.t.Helper()
	h.clk.Advance(d)
	h.clk.BlockUntil(runningWaiters)
}

// finish waits for Run to return.
func (h *harness) finish() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(30 * time.Second):
		// A failsafe, never reached by a passing test: every wait in the
		// supervisor is driven by the fake clock.
		h.t.Fatal("Run did not return")
		return nil
	}
}

// running reports whether Run is still executing.
func (h *harness) running() bool {
	select {
	case err := <-h.done:
		h.done <- err
		return false
	default:
		return true
	}
}

// instanceState reads the recorded state, or "" if the record is gone.
func (h *harness) instanceState() state.InstanceState {
	h.t.Helper()
	inst := h.instanceRecord()
	if inst == nil {
		return ""
	}
	return inst.State
}

func (h *harness) instanceRecord() *state.Instance {
	h.t.Helper()
	lk, err := h.store.RLock(h.projectID)
	if err != nil {
		h.t.Fatalf("rlock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := h.store.ReadInstance(h.projectID)
	if err != nil {
		h.t.Fatalf("read instance: %v", err)
	}
	return inst
}

// addSession registers a session owned by this test process, so that the
// supervisor's liveness check sees it as attached.
func (h *harness) addSession(id string) {
	h.t.Helper()
	pid, start, err := state.Self()
	if err != nil {
		h.t.Fatalf("state.Self: %v", err)
	}
	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	sess := &state.Session{ID: id, PID: pid, ProcStart: start, StartedAt: h.clk.Now()}
	if err := h.store.AddSession(h.projectID, sess); err != nil {
		h.t.Fatalf("add session: %v", err)
	}
}

func (h *harness) removeSession(id string) {
	h.t.Helper()
	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	if err := h.store.RemoveSession(h.projectID, id); err != nil {
		h.t.Fatalf("remove session: %v", err)
	}
}

// projectDirExists reports whether the project's state directory survives.
func (h *harness) projectDirExists() bool {
	_, err := os.Stat(h.store.ProjectDir(h.projectID))
	return err == nil
}

// portOf extracts the port from a host:port address.
func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port %q: %v", p, err)
	}
	return n
}

// freePort returns a loopback port nothing is listening on. The gap between
// closing the probe and the supervisor binding is the same race pt shell has,
// and is what the supervisor's single rebind retry exists for.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := portOf(t, ln.Addr().String())
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

// echoServer starts a loopback listener that echoes what it is sent, standing
// in for a service running inside the guest.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				_, _ = conn.Write(buf[:n])
			}()
		}
	}()
	return portOf(t, ln.Addr().String())
}

// eventually spins until cond holds. It exists only for the one assertion whose
// effect is a file timestamp rather than a clock waiter; it yields rather than
// sleeps, so a passing test spends microseconds here.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}
