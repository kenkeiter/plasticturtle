package state

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// deadlineRecorder is a tart.Client that remembers the context each call got.
type deadlineRecorder struct {
	mu        sync.Mutex
	deadlines map[string]time.Time
	hadNone   map[string]bool
}

func newDeadlineRecorder() *deadlineRecorder {
	return &deadlineRecorder{
		deadlines: map[string]time.Time{},
		hadNone:   map[string]bool{},
	}
}

func (d *deadlineRecorder) record(method string, ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		d.deadlines[method] = dl
	} else {
		d.hadNone[method] = true
	}
}

func (d *deadlineRecorder) deadlineFor(method string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hadNone[method] {
		return time.Time{}, false
	}
	dl, ok := d.deadlines[method]
	return dl, ok
}

func (d *deadlineRecorder) Stop(ctx context.Context, name string, force bool) error {
	d.record("Stop", ctx)
	return nil
}

func (d *deadlineRecorder) Delete(ctx context.Context, name string) error {
	d.record("Delete", ctx)
	return nil
}

func (d *deadlineRecorder) Clone(ctx context.Context, image, name string) error { return nil }
func (d *deadlineRecorder) Set(ctx context.Context, name string, cpu, mem int) error {
	return nil
}
func (d *deadlineRecorder) Run(ctx context.Context, name string, opts tart.RunOpts) (sys.Process, error) {
	return nil, nil
}
func (d *deadlineRecorder) IP(ctx context.Context, name string) (string, error) { return "", nil }
func (d *deadlineRecorder) List(ctx context.Context) ([]tart.VM, error)         { return nil, nil }

// Reclaiming a VM must be bounded, because it runs under the project's
// exclusive lock.
//
// Without a deadline one wedged `tart delete` holds that lock forever and every
// other pt invocation for the project fails. The test asserts the deadline
// rather than waiting for it: a test that actually blocked for ReclaimTimeout
// would take a minute and nobody would run it.
func TestReclaimIsBounded(t *testing.T) {
	rec := newDeadlineRecorder()
	before := time.Now()

	// context.Background() has no deadline of its own, so any deadline the
	// calls see was imposed by forceDeleteVM.
	if err := forceDeleteVM(context.Background(), rec, "pt-0123456789abcdef-89abcdef"); err != nil {
		t.Fatalf("forceDeleteVM: %v", err)
	}

	for _, method := range []string{"Stop", "Delete"} {
		dl, ok := rec.deadlineFor(method)
		if !ok {
			t.Errorf("%s ran with no deadline; a wedged subprocess would hold the project lock forever", method)
			continue
		}
		if budget := dl.Sub(before); budget > ptcfg.ReclaimTimeout+time.Second {
			t.Errorf("%s deadline is %s away, want at most ReclaimTimeout (%s)", method, budget, ptcfg.ReclaimTimeout)
		}
	}
}

// The name guard runs before anything is bounded or executed: a VM that is not
// ours must never reach tart at all.
func TestReclaimRefusesForeignNamesBeforeDialing(t *testing.T) {
	rec := newDeadlineRecorder()
	if err := forceDeleteVM(context.Background(), rec, "my-dev-box"); err == nil {
		t.Error("forceDeleteVM accepted a VM that is not ours")
	}
	if _, ok := rec.deadlineFor("Stop"); ok {
		t.Error("a foreign VM reached tart.Stop")
	}
	if _, ok := rec.deadlineFor("Delete"); ok {
		t.Error("a foreign VM reached tart.Delete")
	}
}
