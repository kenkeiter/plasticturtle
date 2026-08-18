package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// watchSessions tears the instance down once the session set has been empty
// for ptcfg.SessionEmptyDebounce.
//
// The debounce is what makes `exit && pt shell` work: for those few hundred
// milliseconds there is genuinely nobody attached, and without it the VM the
// user is re-entering would be destroyed underneath them. It is measured from
// the first observation of an empty set and re-checked at its end, so a session
// that reappears inside the window resets it rather than merely delaying it.
func (r *run) watchSessions() {
	wait := ptcfg.SessionPollInterval
	var emptySince time.Time

	for {
		select {
		case <-r.stopped:
			return
		case <-r.clk.After(wait):
		}

		live, err := r.liveSessions()
		if err != nil {
			// An unreadable session directory is not evidence that nobody is
			// attached, and destroying a VM on a failed readdir would be the
			// worst possible response to it.
			r.logf("session poll: %v", err)
			wait, emptySince = ptcfg.SessionPollInterval, time.Time{}
			continue
		}
		if live > 0 {
			wait, emptySince = ptcfg.SessionPollInterval, time.Time{}
			continue
		}

		now := r.clk.Now()
		if emptySince.IsZero() {
			emptySince = now
			r.logf("no sessions; teardown in %s unless one returns", ptcfg.SessionEmptyDebounce)
			wait = ptcfg.SessionEmptyDebounce
			continue
		}
		if remaining := ptcfg.SessionEmptyDebounce - now.Sub(emptySince); remaining > 0 {
			wait = remaining
			continue
		}
		if r.claimIdle() {
			r.requestStop(stopIdle, "no sessions remain")
			return
		}
		// A session appeared in the instant between the poll and the claim.
		wait, emptySince = ptcfg.SessionPollInterval, time.Time{}
	}
}

// liveSessions counts the sessions whose process is still alive, dropping the
// records of those that are not.
func (r *run) liveSessions() (int, error) {
	lk, err := r.d.Store.Lock(r.p.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("lock project: %w", err)
	}
	defer func() { _ = lk.Unlock() }()

	sessions, err := r.d.Store.LiveSessions(r.p.ProjectID)
	if err != nil {
		return 0, err
	}
	return len(sessions), nil
}

// claimIdle publishes state stopping in the same lock hold that confirms the
// session set is empty, and reports whether it did.
//
// Splitting those two would leave a window in which a pt shell takes the lock,
// reads state running, and registers a session against a VM this supervisor
// has already decided to destroy. Marking stopping under the same lock closes
// it: a shell arriving one instant later sees stopping and waits for dead
// before creating a fresh instance, which is exactly the behavior spec §10
// asks for.
func (r *run) claimIdle() bool {
	err := r.withInstance(func(inst *state.Instance) error {
		sessions, err := r.d.Store.LiveSessions(r.p.ProjectID)
		if err != nil {
			return err
		}
		if len(sessions) > 0 {
			return errSessionReturned
		}
		inst.State = state.StateStopping
		return nil
	})
	switch {
	case errors.Is(err, errSessionReturned):
		return false
	case err != nil:
		// The mark could not be published. Teardown still has to happen — the
		// session set is empty and nothing else will ever look again — so the
		// claim succeeds and teardown retries the transition itself.
		r.logf("claiming idle instance: %v", err)
		return true
	}
	r.stopping.Store(true)
	return true
}

// errSessionReturned aborts the idle claim from inside the locked mutation. It
// is a control signal, never reported.
var errSessionReturned = errors.New("supervisor: session returned during claim")

// teardown runs the shutdown sequence exactly once.
//
// It is reachable from every watcher and from the boot path, so the once is
// load-bearing — but it is not the only guard: the watchers themselves never
// call this. They call requestStop, and the goroutine that started them runs
// the sequence below after they have all returned. The once is what makes the
// boot-failure path and the normal path able to share this code safely.
func (r *run) teardown(ctx context.Context) {
	r.teardownOnce.Do(func() { r.tearDownOnce(ctx) })
}

func (r *run) tearDownOnce(parent context.Context) {
	// Logged here rather than at the call sites so that one teardown is one
	// line, whichever watcher won the race to ask for it.
	r.logf("teardown: %s", r.stopReason)

	// Deliberately detached from the caller's context: SIGTERM and a cancelled
	// context are two of the reasons we are here, and a teardown that cannot
	// run tart stop because its context is already dead would leak the VM it
	// was invoked to remove.
	ctx := context.WithoutCancel(parent)

	if r.published && !r.stopping.Load() {
		if err := r.setState(state.StateStopping); err != nil {
			r.logf("marking stopping: %v", err)
		}
	}

	// Tunnels first. Closing the transport underneath a live forward would
	// leave its goroutines to discover the failure instead of being told.
	if r.sshc != nil {
		if err := r.sshc.Close(); err != nil {
			r.logf("closing ssh connection: %v", err)
		}
	}

	r.stopVM(ctx)

	if err := r.setState(state.StateDead); err != nil {
		r.logf("marking dead: %v", err)
	}

	deleted := r.deleteClone(ctx)

	// Spec §6.3 step 6: remove the state files only if the clone really went
	// away. A record that outlives its VM is what lets the next pt shell (or
	// GC) find and delete it; removing the record first would orphan the clone
	// under a name nothing remembers.
	if deleted && r.stopKind.removesState() {
		// The heartbeat has to stop first: state.Heartbeat recreates the
		// project directory on its way to touching the file, so a beat landing
		// after the removal would leave exactly the kind of half-populated
		// directory garbage collection then has to reason about.
		if r.stopBeat != nil {
			r.stopBeat()
		}
		if err := r.removeState(); err != nil {
			r.logf("removing state: %v", err)
		}
	}

	// Last: a child that survived both stops has to go, or the supervisor's
	// exit would leave a VM with no owner.
	if r.killVM != nil {
		r.killVM()
	}
}

// stopVM stops the guest gracefully and escalates if it does not go.
func (r *run) stopVM(ctx context.Context) {
	if !r.booted {
		return
	}
	if err := r.d.Tart.Stop(ctx, r.p.InstanceName, false); err != nil {
		// A VM that is already gone reports a failure here; so does one that
		// refused to shut down. Both are answered the same way.
		r.logf("graceful stop: %v", err)
		r.forceStop(ctx)
		return
	}
	select {
	case <-r.childDone:
		r.logf("vm stopped")
	case <-r.clk.After(ptcfg.GracefulStopTimeout):
		r.logf("vm did not stop within %s", ptcfg.GracefulStopTimeout)
		r.forceStop(ctx)
	}
}

func (r *run) forceStop(ctx context.Context) {
	if err := r.d.Tart.Stop(ctx, r.p.InstanceName, true); err != nil {
		r.logf("forced stop: %v", err)
	}
}

// deleteClone removes the VM from disk, reporting whether it is gone.
func (r *run) deleteClone(ctx context.Context) bool {
	if !r.cloned {
		return false
	}
	err := r.d.Tart.Delete(ctx, r.p.InstanceName)
	if err == nil || errors.Is(err, tart.ErrNotFound) {
		r.logf("deleted clone %s", r.p.InstanceName)
		return true
	}
	// Best effort by design: the instance record survives, and the next GC
	// pass finds a dead supervisor and tries again.
	r.logf("deleting clone %s: %v", r.p.InstanceName, err)
	return false
}

// removeState deletes the project's state directory.
func (r *run) removeState() error {
	lk, err := r.d.Store.Lock(r.p.ProjectID)
	if err != nil {
		return fmt.Errorf("lock project: %w", err)
	}
	defer func() { _ = lk.Unlock() }()

	// RemoveProject deletes the lock file this call is holding, which is safe
	// only because the flock lives on the open descriptor — and only as the
	// last mutation before the unlock. Nothing may be written after it.
	return r.d.Store.RemoveProject(r.p.ProjectID)
}
