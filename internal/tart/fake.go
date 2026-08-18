package tart

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kenkeiter/plasticturtle/internal/sys"
)

var _ Client = (*Fake)(nil)

// fakeVM is one VM the Fake believes exists.
type fakeVM struct {
	source    Source
	running   bool
	cpu       int
	memoryMiB int
	dirs      []DirShare
}

// Fake is an in-memory Client for tests. It tracks VM existence and state so
// that lifecycle assertions ("the clone was deleted after teardown") are real
// assertions and not just call-log matching.
//
// It is safe for concurrent use: the supervisor's watchers call it from several
// goroutines at once, and a fake that races would report the supervisor's bugs
// as its own.
type Fake struct {
	mu      sync.Mutex
	vms     map[string]*fakeVM
	ips     map[string]string
	procs   map[string]*sys.FakeProcess
	fail    map[string]error
	nextPID int
}

// NewFake returns a Fake with the given images already present.
func NewFake(images ...string) *Fake {
	f := &Fake{
		vms:   map[string]*fakeVM{},
		ips:   map[string]string{},
		procs: map[string]*sys.FakeProcess{},
		fail:  map[string]error{},
		// Distinct from sys.FakeRunner's range so a stray PID in a test failure
		// points at whichever fake produced it.
		nextPID: 71000,
	}
	for _, img := range images {
		f.vms[img] = &fakeVM{source: sourceOf(img)}
	}
	return f
}

// sourceOf guesses where a name came from the way a reader would: anything
// registry-shaped is an OCI image, everything else is local.
func sourceOf(name string) Source {
	if strings.Contains(name, "/") {
		return SourceOCI
	}
	return SourceLocal
}

// Existing returns the names of VMs the fake currently holds, sorted. Seed
// images count: to tart they are VMs, and a teardown assertion wants to see
// exactly the images it started with and no leftover clone.
func (f *Fake) Existing() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.vms))
	for name := range f.vms {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SetIP sets the address IP will return for name once it is running. Calling it
// before the VM exists is allowed, so a test can arm the answer up front and
// let the supervisor's boot poll find it.
func (f *Fake) SetIP(name, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips[name] = ip
}

// FailNext makes the next call to the named method return err. The method name
// is the Client method ("Clone", "IP", ...), matched case-insensitively. The
// injection is consumed by exactly one call, so a retry loop under test sees a
// single failure followed by success.
func (f *Fake) FailNext(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[strings.ToLower(method)] = err
}

// Process returns the handle previously returned by Run for name, so a test
// can make the VM die unexpectedly. The handle survives Delete: teardown
// assertions run after the clone is gone.
func (f *Fake) Process(name string) *sys.FakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[name]
}

// enter performs the bookkeeping every Client method starts with: honor a
// cancelled context and consume any injected failure. The caller must hold f.mu.
func (f *Fake) enter(ctx context.Context, method string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := strings.ToLower(method)
	if err, ok := f.fail[key]; ok {
		delete(f.fail, key)
		return err
	}
	return nil
}

// vm looks up a VM, reporting the sentinel the real CLI would. The caller must
// hold f.mu.
func (f *Fake) vm(name string) (*fakeVM, error) {
	v, ok := f.vms[name]
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return v, nil
}

func (f *Fake) Clone(ctx context.Context, image, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Clone"); err != nil {
		return err
	}
	if _, err := f.vm(image); err != nil {
		return err
	}
	if _, exists := f.vms[name]; exists {
		return fmt.Errorf("%s: already exists", name)
	}
	f.vms[name] = &fakeVM{source: SourceLocal}
	return nil
}

func (f *Fake) Set(ctx context.Context, name string, cpu, memoryMiB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Set"); err != nil {
		return err
	}
	// Mirror the CLI: an all-zero Set is a no-op that never reaches tart, so it
	// must not fail on a missing VM either.
	if cpu == 0 && memoryMiB == 0 {
		return nil
	}
	v, err := f.vm(name)
	if err != nil {
		return err
	}
	if cpu != 0 {
		v.cpu = cpu
	}
	if memoryMiB != 0 {
		v.memoryMiB = memoryMiB
	}
	return nil
}

func (f *Fake) Run(ctx context.Context, name string, opts RunOpts) (sys.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Run"); err != nil {
		return nil, err
	}
	v, err := f.vm(name)
	if err != nil {
		return nil, err
	}
	if v.running {
		return nil, fmt.Errorf("%s: already running", name)
	}
	v.running = true
	v.dirs = append([]DirShare(nil), opts.Dirs...)
	p := sys.NewFakeProcess(f.nextPID)
	f.nextPID++
	f.procs[name] = p
	return p, nil
}

func (f *Fake) Stop(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Stop"); err != nil {
		return err
	}
	v, err := f.vm(name)
	if err != nil {
		return err
	}
	// Idempotent on purpose: teardown stops gracefully and then again with
	// force, and the second call must not look like a failure.
	f.halt(name, v)
	return nil
}

func (f *Fake) Delete(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "Delete"); err != nil {
		return err
	}
	v, err := f.vm(name)
	if err != nil {
		return err
	}
	f.halt(name, v)
	delete(f.vms, name)
	delete(f.ips, name)
	return nil
}

// halt marks a VM stopped and releases anyone waiting on its run handle, which
// is what a real `tart stop` does to the `tart run` child. The caller must hold
// f.mu.
func (f *Fake) halt(name string, v *fakeVM) {
	if !v.running {
		return
	}
	v.running = false
	if p := f.procs[name]; p != nil {
		p.Exit(nil)
	}
}

func (f *Fake) IP(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "IP"); err != nil {
		return "", err
	}
	v, err := f.vm(name)
	if err != nil {
		return "", err
	}
	// A stopped guest holds no lease, so it is ErrNoIP and not an error the
	// boot poll should abort on.
	if !v.running || f.ips[name] == "" {
		return "", fmt.Errorf("%s: %w", name, ErrNoIP)
	}
	return f.ips[name], nil
}

func (f *Fake) List(ctx context.Context) ([]VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.enter(ctx, "List"); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f.vms))
	for name := range f.vms {
		names = append(names, name)
	}
	sort.Strings(names)

	// DiskGB and SizeGB stay zero: the fake models existence and state, which
	// is what lifecycle logic branches on, and inventing plausible disk numbers
	// would only invite a test to assert them.
	out := make([]VM, 0, len(names))
	for _, name := range names {
		v := f.vms[name]
		state := StateStopped
		if v.running {
			state = StateRunning
		}
		out = append(out, VM{Source: v.source, Name: name, State: state})
	}
	return out, nil
}
