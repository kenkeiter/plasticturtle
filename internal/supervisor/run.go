package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
)

// stopKind is why teardown was asked for. It decides one thing: whether the
// project's state directory survives.
//
// A deliberate shutdown leaves nothing worth reading, so its state files go.
// A VM that died on its own, or one that never booted, leaves a supervisor.log
// that is the only explanation the user will ever get — and an instance record
// in state dead, which is what tells a pt shell still waiting on this instance
// that it is not coming.
type stopKind int

const (
	stopIdle stopKind = iota
	stopSignal
	stopVMDied
	stopBootFailed
)

// removesState reports whether teardown may delete the project's state
// directory once the clone is gone.
func (k stopKind) removesState() bool { return k == stopIdle || k == stopSignal }

// sshPort is the guest port sshd listens on. It is a variable, not a constant,
// only so that tests can point the boot path at an in-process SSH server on a
// loopback address; nothing configures it at runtime.
var sshPort = 22

// run is one instance's lifecycle. Exactly one of these exists per supervisor
// process. Every field is written by the main goroutine before the watchers
// start and read after they have all returned, except the three groups called
// out below, which are the whole of this package's concurrency.
type run struct {
	p   *Params
	d   Deps
	clk sys.Clock

	// stopOnce/stopped/stopKind/stopReason are the single concurrent entry
	// point into teardown. Every watcher — the session poll, the tart run
	// child, the signal handler — races to call requestStop; the once decides
	// which reason wins, and closing the channel wakes all the others. The
	// teardown *sequence* itself then runs on one goroutine only.
	stopOnce   sync.Once
	stopped    chan struct{}
	stopKind   stopKind
	stopReason string

	teardownOnce sync.Once

	// childDone is closed when the tart run child exits, whichever of boot,
	// the watchers or teardown is waiting on it. Closing rather than sending
	// means every waiter sees it and none consumes it.
	childDone chan struct{}
	childErr  error

	sshc *sshx.Client

	// killVM hard-kills the tart run child, and stopBeat halts the heartbeat.
	// Both are set by execute and are nil in unit tests that drive teardown
	// directly.
	killVM   context.CancelFunc
	stopBeat func()

	vmIP      string
	vmPID     int
	cloned    bool
	booted    bool
	published bool

	// stopping is set by the session watcher when it publishes state stopping
	// itself, under the same lock in which it confirmed the session set was
	// empty. Teardown reads it from another goroutine, hence the atomic.
	stopping atomic.Bool
}

func (r *run) logf(format string, args ...any) { r.d.Logf(format, args...) }

// requestStop records why the instance is going away and wakes every watcher.
//
// This is the whole of teardown's concurrency control. Any number of goroutines
// may call it at any time; the first one to arrive names the reason, and the
// rest return immediately. Nothing else in this package closes r.stopped.
func (r *run) requestStop(kind stopKind, format string, args ...any) {
	r.stopOnce.Do(func() {
		r.stopKind = kind
		r.stopReason = fmt.Sprintf(format, args...)
		close(r.stopped)
	})
}

// execute is the body of Run.
func (r *run) execute(ctx context.Context) error {
	// The tart run child gets a context that teardown cannot cancel: with
	// exec.CommandContext underneath, cancelling it kills the VM outright,
	// which is precisely what the graceful stop below is trying to avoid. It
	// is cancelled last, as a backstop for a child that outlived tart stop.
	vmCtx, killVM := context.WithCancel(context.WithoutCancel(ctx))
	r.killVM = killVM
	defer killVM()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := r.claim(); err != nil {
		return err
	}

	// The heartbeat starts before the boot and outlives the watchers: a 120 s
	// boot and a 30 s stop are both long enough for a reader to mistake a
	// silent supervisor for a dead one. It stops before the state directory is
	// removed, because a beat arriving afterwards would recreate the directory
	// teardown just deleted.
	beatDone := make(chan struct{})
	var beats sync.WaitGroup
	beats.Add(1)
	go func() { defer beats.Done(); r.heartbeat(beatDone) }()
	r.stopBeat = sync.OnceFunc(func() { close(beatDone); beats.Wait() })
	defer r.stopBeat()

	// The signal watcher runs from here rather than from the running state:
	// "tear down now" has to be answerable during a two-minute boot, which is
	// exactly when a user is most likely to give up and send it.
	var watchers sync.WaitGroup
	watchers.Add(1)
	go func() { defer watchers.Done(); r.watchSignals(ctx, sigCh) }()

	if err := r.boot(ctx, vmCtx); err != nil {
		r.requestStop(stopBootFailed, "boot failed: %v", err)
		watchers.Wait()
		r.teardown(ctx)
		return err
	}

	if err := r.publish(); err != nil {
		r.requestStop(stopBootFailed, "publishing running state failed: %v", err)
		watchers.Wait()
		r.teardown(ctx)
		return err
	}
	r.logf("instance %s is running at %s", r.p.InstanceName, r.vmIP)

	// The second watcher, alongside the signal watcher already running and the
	// tart run child's own goroutine started in boot. All three funnel into
	// requestStop; none of them tears anything down itself.
	watchers.Add(1)
	go func() { defer watchers.Done(); r.watchSessions() }()
	watchers.Wait()

	r.teardown(ctx)
	if r.stopKind == stopVMDied {
		// Reported as an error so it reaches supervisor.log with a non-zero
		// exit status; an attached shell has already seen its SSH drop.
		if r.childErr != nil {
			return fmt.Errorf("supervisor: vm terminated unexpectedly: %w", r.childErr)
		}
		return errors.New("supervisor: vm terminated unexpectedly")
	}
	return nil
}

// claim takes ownership of the project's instance record: it records this
// process as the supervisor and confirms nobody else already is.
//
// pt shell writes the record before spawning us and fills in our PID as soon
// as the spawn returns (so that garbage collection has something to check),
// but that write races this one and cannot know our start ticks. Writing them
// from inside the process that owns them is what closes the window where a
// healthy instance looks unsupervised.
func (r *run) claim() error {
	pid, start, err := state.Self()
	if err != nil {
		return fmt.Errorf("supervisor: identify self: %w", err)
	}
	return r.withInstance(func(inst *state.Instance) error {
		if inst.InstanceName != "" && inst.InstanceName != r.p.InstanceName {
			// Overwriting would orphan whatever VM the record names: the name
			// is the only handle anything has on it.
			return fmt.Errorf("supervisor: project %s already has instance %q, refusing to supervise %q",
				r.p.ProjectID, inst.InstanceName, r.p.InstanceName)
		}
		if inst.SupervisorPID > 0 && inst.SupervisorPID != pid && state.Alive(inst.SupervisorPID, inst.SupervisorStart) {
			return fmt.Errorf("supervisor: instance %s is already supervised by pid %d",
				r.p.InstanceName, inst.SupervisorPID)
		}
		inst.InstanceName = r.p.InstanceName
		inst.ProjectPath = r.p.Config.ProjectPath
		inst.ConfigHash = r.p.ConfigHash
		inst.Ports = portMaps(r.p.Ports)
		inst.SupervisorPID, inst.SupervisorStart = pid, start
		if inst.State == "" {
			inst.State = state.StateCreating
		}
		if inst.CreatedAt.IsZero() {
			inst.CreatedAt = r.clk.Now()
		}
		return nil
	})
}

// publish moves the instance to running with the address the tunnels are
// using. This is the transition a waiting pt shell is polling for, so nothing
// may set it before every forward is actually listening.
func (r *run) publish() error {
	r.published = true
	return r.withInstance(func(inst *state.Instance) error {
		inst.State = state.StateRunning
		inst.VMIP = r.vmIP
		inst.VMPID = r.vmPID
		inst.Ports = portMaps(r.p.Ports)
		return nil
	})
}

// setState records a lifecycle transition.
func (r *run) setState(s state.InstanceState) error {
	return r.withInstance(func(inst *state.Instance) error {
		inst.State = s
		return nil
	})
}

// withInstance applies mutate to the project's instance record under the
// exclusive lock.
//
// The lock is held for exactly one read, one mutation and one write. Nothing
// slow — no boot, no dial, no tart stop — happens inside it, because every
// other pt invocation for this project is blocked while it is held.
func (r *run) withInstance(mutate func(*state.Instance) error) error {
	lk, err := r.d.Store.Lock(r.p.ProjectID)
	if err != nil {
		return fmt.Errorf("supervisor: lock project: %w", err)
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.p.ProjectID)
	if err != nil {
		return fmt.Errorf("supervisor: read instance record: %w", err)
	}
	if inst == nil {
		// The record is normally written by pt shell before we are spawned.
		// Rebuilding it from the parameters rather than failing keeps a VM we
		// may already have booted nameable — an instance record is the only
		// thing that stops garbage collection treating a clone as an orphan.
		inst = &state.Instance{
			InstanceName: r.p.InstanceName,
			ProjectPath:  r.p.Config.ProjectPath,
			ConfigHash:   r.p.ConfigHash,
			State:        state.StateCreating,
			CreatedAt:    r.clk.Now(),
			Ports:        portMaps(r.p.Ports),
		}
	}
	if err := mutate(inst); err != nil {
		return err
	}
	if err := r.d.Store.WriteInstance(r.p.ProjectID, inst); err != nil {
		return fmt.Errorf("supervisor: write instance record: %w", err)
	}
	// Every write is logged, not only the transitions: supervisor.log is the
	// only record of what this instance did, and "which state was it in when
	// that happened" is the first question anyone reading it asks.
	r.logf("state: %s", inst.State)
	return nil
}

// heartbeat touches the heartbeat file until done is closed.
func (r *run) heartbeat(done <-chan struct{}) {
	tk := r.clk.NewTicker(ptcfg.HeartbeatInterval)
	defer tk.Stop()

	r.beat()
	for {
		select {
		case <-done:
			return
		case <-tk.C():
			r.beat()
		}
	}
}

func (r *run) beat() {
	if err := r.d.Store.Heartbeat(r.p.ProjectID); err != nil {
		// A missed beat degrades pt list to "stale" and nothing more, so it is
		// reported and not escalated: tearing a healthy VM down because a file
		// timestamp could not be written would be a far worse trade.
		r.logf("heartbeat: %v", err)
	}
}

// watchSignals turns SIGTERM — and a cancelled context, which is the same
// request arriving through the API — into a teardown.
func (r *run) watchSignals(ctx context.Context, sigCh <-chan os.Signal) {
	select {
	case <-r.stopped:
	case sig := <-sigCh:
		r.requestStop(stopSignal, "received %s", sig)
	case <-ctx.Done():
		r.requestStop(stopSignal, "context cancelled: %v", ctx.Err())
	}
}

// portMaps converts the resolved forwards into the on-disk shape pt ports
// renders.
func portMaps(resolved []ports.Resolved) []state.PortMap {
	out := make([]state.PortMap, 0, len(resolved))
	for _, p := range resolved {
		out = append(out, state.PortMap{
			VMPort:           p.VMPort,
			HostPort:         p.HostPort,
			OriginalHostPort: p.OriginalHostPort,
		})
	}
	return out
}
