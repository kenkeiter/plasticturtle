package state

import (
	"context"
	"errors"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// fakeTart is this package's own tart.Client double. internal/tart.Fake is
// implemented by another agent; depending on it would couple this package's
// tests to that one's schedule, and GC needs only existence tracking anyway.
type fakeTart struct {
	mu        sync.Mutex
	vms       map[string]tart.VM
	stopped   []string
	deleted   []string
	listErr   error
	deleteErr error
}

func newFakeTart(vms ...tart.VM) *fakeTart {
	f := &fakeTart{vms: map[string]tart.VM{}}
	for _, vm := range vms {
		f.vms[vm.Name] = vm
	}
	return f
}

func localVM(name string) tart.VM {
	return tart.VM{Source: tart.SourceLocal, Name: name, State: tart.StateRunning}
}

func (f *fakeTart) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.vms))
	for name := range f.vms {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (f *fakeTart) List(ctx context.Context) ([]tart.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]tart.VM, 0, len(f.vms))
	for _, vm := range f.vms {
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeTart) Stop(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	vm, ok := f.vms[name]
	if !ok {
		return tart.ErrNotFound
	}
	if vm.State != tart.StateRunning {
		// Real tart fails on an already-stopped VM; GC must tolerate it.
		return errors.New("tart: vm is not running")
	}
	vm.State = tart.StateStopped
	f.vms[name] = vm
	return nil
}

func (f *fakeTart) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.vms[name]; !ok {
		return tart.ErrNotFound
	}
	delete(f.vms, name)
	return nil
}

func (f *fakeTart) Clone(ctx context.Context, image, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[name] = localVM(name)
	return nil
}

func (f *fakeTart) Set(ctx context.Context, name string, cpu, memoryMiB int) error { return nil }

func (f *fakeTart) Run(ctx context.Context, name string, opts tart.RunOpts) (sys.Process, error) {
	return nil, errors.New("fakeTart: Run is not used by GC")
}

func (f *fakeTart) IP(ctx context.Context, name string) (string, error) {
	return "", tart.ErrNoIP
}

// deadInstance builds a record whose supervisor cannot possibly be running.
func deadInstance(t *testing.T, projectPath string, state InstanceState) (string, *Instance) {
	t.Helper()
	id := ProjectID(projectPath)
	name, err := NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}
	return id, &Instance{
		InstanceName:    name,
		ProjectPath:     projectPath,
		State:           state,
		SupervisorPID:   1 << 30,
		SupervisorStart: 999999,
		CreatedAt:       time.Now().Add(-time.Hour),
	}
}

// liveInstance builds a record supervised by this test process.
func liveInstance(t *testing.T, projectPath string) (string, *Instance) {
	t.Helper()
	id := ProjectID(projectPath)
	name, err := NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}
	pid, start, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	return id, &Instance{
		InstanceName:    name,
		ProjectPath:     projectPath,
		State:           StateRunning,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       time.Now(),
	}
}

func TestGCReclaimsDeadSupervisor(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/dead", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if got := tc.names(); len(got) != 0 {
		t.Fatalf("VMs left after GC: %v", got)
	}
	if len(tc.stopped) == 0 || tc.stopped[0] != inst.InstanceName {
		t.Fatalf("VM was deleted without a force stop: stopped=%v", tc.stopped)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir survived GC: %v", err)
	}
}

// TestGCReclaimsDeadStateWithDeadSupervisor covers the spec's "Dead -> NoInstance
// ... or pt GC" edge: a finished instance whose supervisor is gone still has a
// clone on disk.
func TestGCReclaimsDeadStateWithDeadSupervisor(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/finished", StateDead)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := tc.names(); len(got) != 0 {
		t.Fatalf("VMs left after GC: %v", got)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir survived GC: %v", err)
	}
}

func TestGCKeepsLiveSupervisorAndPrunesDeadSessions(t *testing.T) {
	s := newTestStore(t)
	id, inst := liveInstance(t, "/Users/alice/live")
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSession(id, selfSession(t, "alive")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSession(id, deadSession("dead")); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}

	if got := tc.names(); len(got) != 1 || got[0] != inst.InstanceName {
		t.Fatalf("GC touched a live supervisor's VM: %v", got)
	}
	got, err := s.ReadInstance(id)
	if err != nil || got == nil {
		t.Fatalf("instance record lost: (%v, %v)", got, err)
	}
	sessions, err := s.ListSessions(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "alive" {
		t.Fatalf("sessions after GC = %v, want only the live one", sessions)
	}
}

// TestGCNeverTouchesForeignVMs is the safety boundary. Everything here is
// somebody else's property.
func TestGCNeverTouchesForeignVMs(t *testing.T) {
	s := newTestStore(t)
	foreign := []string{
		"my-dev-box",
		"pt-notours",
		"pt",
		"ptx-0123456789abcdef-01234567",
		"pt-0123456789abcdef-0123456",    // 7 hex, one short
		"pt-0123456789abcdef-012345678",  // 9 hex, one long
		"pt-0123456789abcdeg-01234567",   // 'g' is not hex
		"pt-0123456789ABCDEF-01234567",   // uppercase
		"pt-0123456789abcdef-01234567-x", // trailing junk
		"xpt-0123456789abcdef-01234567",  // leading junk
		"pt-0123456789abcdef_01234567",   // wrong separator
		"macos-tahoe-base",
	}
	vms := make([]tart.VM, 0, len(foreign))
	for _, name := range foreign {
		vms = append(vms, localVM(name))
	}
	// A cached OCI image that somehow carries our name shape is still not a
	// clone we made.
	vms = append(vms, tart.VM{Source: tart.SourceOCI, Name: "pt-0123456789abcdef-01234567", State: tart.StateStopped})
	tc := newFakeTart(vms...)

	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if len(tc.deleted) != 0 {
		t.Fatalf("GC deleted foreign VMs: %v", tc.deleted)
	}
	if len(tc.stopped) != 0 {
		t.Fatalf("GC stopped foreign VMs: %v", tc.stopped)
	}
	if len(tc.names()) != len(foreign)+1 {
		t.Fatalf("VMs after GC = %v", tc.names())
	}
}

func TestGCDeletesOrphanedVM(t *testing.T) {
	s := newTestStore(t)
	orphan, err := NewInstanceName(ProjectID("/Users/alice/vanished"))
	if err != nil {
		t.Fatal(err)
	}

	// A second project that exists but whose record names a *different* VM:
	// the old clone is just as orphaned as one with no state dir at all.
	id, inst := liveInstance(t, "/Users/alice/live")
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	stale, err := NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}

	tc := newFakeTart(localVM(orphan), localVM(stale), localVM(inst.InstanceName), localVM("my-dev-box"))
	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}

	got := tc.names()
	want := []string{"my-dev-box", inst.InstanceName}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("VMs after GC = %v, want %v", got, want)
	}
}

func TestGCSkipsLockedProject(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/busy", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	lk, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()

	// GC must give up on a busy project quickly: `pt list` runs this pass.
	start := time.Now()
	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if elapsed := time.Since(start); elapsed > ptcfg.LockTimeout/2 {
		t.Fatalf("GC blocked for %v behind a locked project", elapsed)
	}

	if got := tc.names(); len(got) != 1 {
		t.Fatalf("GC acted on a locked project: VMs = %v", got)
	}
	if got, err := s.ReadInstance(id); err != nil || got == nil {
		t.Fatalf("GC removed a locked project's record: (%v, %v)", got, err)
	}
}

// TestGCSpareCreatingInstanceWithoutSupervisor covers the window the spec's own
// ordering opens: pt shell writes the record, then spawns the supervisor.
func TestGCSparesCreatingInstanceWithoutSupervisor(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/Users/alice/booting")
	name, err := NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{InstanceName: name, State: StateCreating, CreatedAt: time.Now()}
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(name))

	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := tc.names(); len(got) != 1 {
		t.Fatalf("GC destroyed a booting instance's VM: %v", got)
	}
	if got, err := s.ReadInstance(id); err != nil || got == nil {
		t.Fatalf("GC removed a booting instance's record: (%v, %v)", got, err)
	}

	// Past the boot timeout, nothing is ever going to claim it.
	inst.CreatedAt = time.Now().Add(-ptcfg.BootTimeout - time.Minute)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	if err := s.GC(context.Background(), tc); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := tc.names(); len(got) != 0 {
		t.Fatalf("GC left a stuck creating instance's VM: %v", got)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir survived GC: %v", err)
	}
}

func TestGCRemovesEmptyProjectDir(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/Users/alice/leftover")
	// A project dir with a lock file and a dead session but no record: the
	// residue of a crash between locking and writing.
	if err := s.AddSession(id, deadSession("dead")); err != nil {
		t.Fatal(err)
	}
	if err := s.GC(context.Background(), newFakeTart()); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty project dir survived GC: %v", err)
	}
}

func TestGCKeepsRecordlessProjectWithLiveSessions(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/Users/alice/attaching")
	if err := s.AddSession(id, selfSession(t, "alive")); err != nil {
		t.Fatal(err)
	}
	if err := s.GC(context.Background(), newFakeTart()); err != nil {
		t.Fatalf("GC: %v", err)
	}
	sessions, err := s.ListSessions(id)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("GC removed a live session: (%v, %v)", sessions, err)
	}
}

// TestGCProjectRefusesForeignInstanceName covers a corrupted or hand-edited
// record: the record is ours, the VM it names is not.
func TestGCProjectRefusesForeignInstanceName(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/Users/alice/tampered")
	inst := &Instance{
		InstanceName:    "my-dev-box",
		State:           StateRunning,
		SupervisorPID:   1 << 30,
		SupervisorStart: 999999,
		CreatedAt:       time.Now().Add(-time.Hour),
	}
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM("my-dev-box"))

	if err := s.GC(context.Background(), tc); err == nil {
		t.Fatal("GC silently accepted a record naming a foreign VM")
	}
	if len(tc.deleted) != 0 || len(tc.stopped) != 0 {
		t.Fatalf("GC acted on a foreign VM: deleted=%v stopped=%v", tc.deleted, tc.stopped)
	}
	// The record is kept: it is the only thing that still explains the state.
	if got, err := s.ReadInstance(id); err != nil || got == nil {
		t.Fatalf("GC removed the record anyway: (%v, %v)", got, err)
	}
}

func TestGCKeepsRecordWhenDeleteFails(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/stubborn", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))
	tc.deleteErr = errors.New("tart: disk is busy")

	if err := s.GC(context.Background(), tc); err == nil {
		t.Fatal("GC reported success despite a failed delete")
	}
	// Losing the record would orphan the VM under a name nothing remembers.
	got, err := s.ReadInstance(id)
	if err != nil || got == nil {
		t.Fatalf("record dropped after a failed delete: (%v, %v)", got, err)
	}
}

func TestGCSurvivesUnparseableRecord(t *testing.T) {
	s := newTestStore(t)
	good, inst := deadInstance(t, "/Users/alice/good", StateRunning)
	if err := s.WriteInstance(good, inst); err != nil {
		t.Fatal(err)
	}
	bad := ProjectID("/Users/alice/corrupt")
	if err := os.MkdirAll(s.ProjectDir(bad), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.InstancePath(bad), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	// One broken project must not stop the sweep.
	if err := s.GC(context.Background(), tc); err == nil {
		t.Fatal("GC hid a corrupt record")
	}
	if got := tc.names(); len(got) != 0 {
		t.Fatalf("GC skipped the healthy project: %v", got)
	}
	if _, err := os.Stat(s.InstancePath(bad)); err != nil {
		t.Fatalf("GC deleted a record it could not read: %v", err)
	}
}

func TestGCWithNilTartClient(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/notart", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	// A caller with no hypervisor access can still collect state records.
	if err := s.GC(context.Background(), nil); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir survived GC: %v", err)
	}
}

func TestGCHonorsCanceledContext(t *testing.T) {
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/canceled", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.GC(ctx, newFakeTart(localVM(inst.InstanceName))); !errors.Is(err, context.Canceled) {
		t.Fatalf("GC on a canceled context = %v, want context.Canceled", err)
	}
}

func TestGCProjectRequiresCallerLock(t *testing.T) {
	// GCProject is the variant pt shell calls with the lock already held; it
	// must not try to take the lock again (which would deadlock nothing on
	// darwin flock semantics, but would be a lie about the contract).
	s := newTestStore(t)
	id, inst := deadInstance(t, "/Users/alice/held", StateRunning)
	if err := s.WriteInstance(id, inst); err != nil {
		t.Fatal(err)
	}
	tc := newFakeTart(localVM(inst.InstanceName))

	lk, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.GCProject(context.Background(), tc, id) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GCProject: %v", err)
		}
	case <-time.After(ptcfg.LockTimeout):
		t.Fatal("GCProject blocked on a lock its caller already holds")
	}
	_ = lk.Unlock()

	if got := tc.names(); len(got) != 0 {
		t.Fatalf("VMs after GCProject: %v", got)
	}
}
