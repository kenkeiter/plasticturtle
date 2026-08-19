package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/supervisor"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// Exit statuses Run reports for its own failures. The remote shell's status is
// passed through untouched and may be anything.
const (
	// exitFailure is pt's own "no".
	exitFailure = 1

	// exitTransport mirrors ssh(1): 255 means the session never happened, as
	// distinct from a remote command that ran and failed.
	exitTransport = 255
)

// untrustedMessage is spec §5's wording, verbatim. It deliberately does not
// distinguish "never allowed" from "changed since it was allowed": telling an
// attacker which of the two they are looking at buys them something, and
// telling the user costs them nothing — the remedy is identical.
const untrustedMessage = ".plasticturtle has changed (or was never allowed). Review it, then run: pt allow"

// configDriftNote is spec §6.2's note, verbatim. Attaching is still the right
// answer: the VM's mounts and image were fixed when it booted, and destroying a
// colleague's live session to pick up an edit would be far worse than a line of
// explanation.
const configDriftNote = "note: config changed since this VM started; changes apply after all shells exit."

// The two halves of a --persist mismatch. An instance's ephemerality is fixed
// when it boots, so a shell that arrives with a different opinion is told which
// kind of VM it actually got rather than being refused.
//
// The second note is the more important one: a user who did not ask for
// persistence is about to make changes that outlive their session, and nothing
// else on the way in would tell them.
const (
	persistLateNote = "note: this project's VM is already running as a throwaway clone; --persist applies to the next one, once every shell has exited."
	persistOnNote   = "note: this VM was started with --persist; changes you make inside it are kept."
)

// superviseCommand is the hidden subcommand pt re-executes itself with. The
// supervisor is this same binary, so there is nothing else to find on PATH.
const superviseCommand = "_supervise"

// sshPort is the guest port sshd listens on. It is a variable, not a constant,
// only so that tests can point an attach at an in-process SSH server on a
// loopback address; nothing configures it at runtime.
var sshPort = 22

// maxAttempts bounds the decide/create/attach loop.
//
// Each iteration is entered only because the world changed underneath the
// previous one — another shell created the instance while we were prompting,
// or the supervisor condemned it between our read and our session
// registration. Those are real races that retrying resolves, but a project
// that produces them without end is wedged, and spinning forever on it would
// be indistinguishable from a hang.
const maxAttempts = 8

// decision is what the locked look at the project's state concluded.
type decision int

const (
	// decCreate: there is no instance, so this shell makes one.
	decCreate decision = iota
	// decWait: an instance exists and is (or is becoming) usable.
	decWait
	// decWaitGone: an instance is on its way out; wait, then make a fresh one.
	decWaitGone
)

// runner is one pt shell invocation. Everything Run needs that is derived
// rather than injected lives here, so no helper takes eight arguments.
type runner struct {
	o   Opts
	d   Deps
	clk sys.Clock

	// msg is where every human-facing line goes — prompts, the spinner, the
	// config-drift note. Stderr, not stdout: the user's remote shell owns the
	// terminal for the rest of the session, and anything pt says about its own
	// bookkeeping should stay out of a redirected stdout.
	msg io.Writer

	projectDir string
	projectID  string
	cfg        *config.Config
	hash       string
}

func newRunner(o Opts, d Deps) (*runner, error) {
	if d.Store == nil {
		return nil, errors.New("shell: no state store")
	}
	if d.Trust == nil {
		return nil, errors.New("shell: no trust store")
	}
	if d.Tart == nil {
		return nil, errors.New("shell: no tart client")
	}
	if d.Clock == nil {
		d.Clock = sys.RealClock()
	}
	if d.Spawn == nil {
		d.Spawn = RealSpawner()
	}
	if d.Creds.User == "" {
		d.Creds = sshx.DefaultCredentials()
	}
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	return &runner{o: o, d: d, clk: d.Clock, msg: o.Err}, nil
}

func (r *runner) run(ctx context.Context) (int, error) {
	// The signal handler is installed before anything is registered on disk.
	// Cancelling the context is what unblocks the boot wait and closes the SSH
	// session, so every path out of this function — signal included — reaches
	// the deferred deregistration below rather than dying with the session
	// record still on disk. A session record that outlives its process pins the
	// VM open until garbage collection notices, which is the worst failure this
	// package can produce.
	ctx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if err := r.resolveProject(); err != nil {
		return exitFailure, err
	}

	inst, sess, err := r.attach(ctx)
	if err != nil {
		return exitFailure, err
	}
	// Deferred, so it also runs while a panic unwinds. Nothing between here and
	// the return may skip it.
	defer r.deregister(sess)

	if inst.ConfigHash != r.hash {
		fmt.Fprintln(r.msg, configDriftNote)
	}
	switch {
	case r.o.Persist && !inst.Persist:
		fmt.Fprintln(r.msg, persistLateNote)
	case inst.Persist:
		fmt.Fprintln(r.msg, persistOnNote)
	}
	return r.session(ctx, inst)
}

// resolveProject finds the project, parses its config, and verifies that these
// exact bytes are allowed at this exact path.
//
// This is the security choke point. An unallowed or altered config is a hard
// error and never a prompt: a prompt here would train users to approve a file
// they have not read, which is the entire failure mode `pt allow` exists to
// prevent.
func (r *runner) resolveProject() error {
	dir, err := config.Find(r.o.Path)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			start := r.o.Path
			if start == "" {
				start = "."
			}
			return fmt.Errorf("%w at or above %s; run pt init to create one", err, start)
		}
		return err
	}
	cfg, raw, err := config.Load(dir)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, config.FileName), err)
	}

	hash := config.HashBytes(raw)
	allowed, err := r.d.Trust.Check(dir, hash)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New(untrustedMessage)
	}

	r.projectDir, r.cfg, r.hash = dir, cfg, hash
	r.projectID = state.ProjectID(dir)
	return nil
}

// attach returns a running instance with this shell's session registered
// against it.
//
// The loop exists for two races that are unavoidable given that the lock is
// never held across a prompt, a spawn or a boot: another shell may create the
// instance while this one is negotiating ports, and the supervisor may condemn
// an idle instance between the read that saw it running and the registration
// that would have kept it alive. Both are resolved by looking again.
func (r *runner) attach(ctx context.Context) (*state.Instance, *state.Session, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		dec, err := r.decide(ctx)
		if err != nil {
			return nil, nil, err
		}
		switch dec {
		case decWaitGone:
			if err := r.waitForGone(ctx); err != nil {
				return nil, nil, err
			}
			continue
		case decCreate:
			// A seam for the concurrency test only; nil in every real build.
			// The window this bug lived in — between deciding to create and
			// claiming the project — is microseconds wide, so a test that
			// merely starts goroutines and hopes they collide does not reliably
			// reproduce it, and a test that cannot fail is worse than none.
			if hookAfterDecideCreate != nil {
				hookAfterDecideCreate()
			}
			created, err := r.create(ctx)
			if err != nil {
				return nil, nil, err
			}
			if !created {
				// Somebody else won the race while we were prompting.
				continue
			}
		case decWait:
		}

		inst, err := r.waitForRunning(ctx)
		if err != nil {
			return nil, nil, err
		}
		sess, err := r.register(inst)
		if err != nil {
			return nil, nil, err
		}
		if sess == nil {
			// The instance was condemned between the read and the claim.
			continue
		}
		return inst, sess, nil
	}
	return nil, nil, fmt.Errorf("shell: the instance for %s kept changing state; try again", r.projectDir)
}

// decide garbage-collects the project and reports what to do next. It holds the
// exclusive lock for the whole look, and for nothing else.
func (r *runner) decide(ctx context.Context) (decision, error) {
	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = lk.Unlock() }()

	// GC is what makes every branch below sound: it is the only thing that
	// reclaims a record whose supervisor died, so without it a crashed boot
	// would leave this shell waiting out the full boot timeout for a process
	// that no longer exists. A GC that fails is therefore fatal rather than
	// skipped — proceeding would mean deciding from state nobody vouches for.
	if err := r.d.Store.GCProject(ctx, r.d.Tart, r.projectID); err != nil {
		return 0, fmt.Errorf("shell: collecting stale state for %s: %w", r.projectDir, err)
	}

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil {
		return 0, err
	}
	if inst == nil {
		return decCreate, nil
	}
	switch inst.State {
	case state.StateStopping, state.StateDead:
		// GC leaves these alone while their supervisor is still alive, which is
		// exactly the teardown-in-progress window. Creating now would race the
		// tart delete that is about to happen.
		return decWaitGone, nil
	default:
		// Running, creating, or a record written by a version that used a state
		// this one does not know. All of them mean "an instance exists"; the
		// boot wait sorts out whether it ever becomes usable.
		return decWait, nil
	}
}

// create negotiates ports, publishes the creating record and spawns the
// supervisor. It reports false when another shell created the instance first,
// in which case nothing was spawned and the caller re-decides.
func (r *runner) create(ctx context.Context) (bool, error) {
	// Item 2 of the implementation plan: mount sources are validated here,
	// while there is still a terminal to report them on. The supervisor checks
	// again, but its complaint would land in a log file nobody is watching yet.
	resolved, err := r.cfg.Resolve(r.projectDir)
	if err != nil {
		return false, err
	}

	// Checked here for the same reason the mounts are: the supervisor would
	// find the same problems, but its complaint lands in a log file nobody is
	// watching, and every one of these has a fix the user has to type.
	if r.o.Persist {
		if err := r.preflightPersist(ctx, resolved.Image); err != nil {
			return false, err
		}
	}

	name, err := state.NewInstanceName(r.projectID)
	if err != nil {
		return false, err
	}
	// The instance is always named, even when no clone will carry that name:
	// it identifies the record, not the VM. vmName is what tart is told.
	vmName := name
	if r.o.Persist {
		vmName = resolved.Image
	}

	// Claim the project BEFORE negotiating ports, not after.
	//
	// Negotiating first meant two simultaneous first-run shells both probed the
	// host ports: the first bound them, and the second saw its own sibling's
	// probe as EADDRINUSE and asked the user to resolve a conflict that did not
	// exist. The loser then discarded the answer and attached to the winner's
	// instance anyway. Claiming first means only one shell ever reaches the
	// prompt, and the loser returns silently.
	claimed, err := r.claim(name, vmName)
	if err != nil || !claimed {
		return false, err
	}

	// From here on the claim is ours, so every failure path must release it.
	// Otherwise the next pt shell waits out the whole boot timeout for a
	// supervisor that was never spawned.
	release := func(err error) (bool, error) {
		r.abandon(name)
		return false, err
	}

	// The lock is not held across Negotiate: it prompts, and a prompt behind
	// the project lock would block every other pt invocation for as long as the
	// user takes to answer. The claim above is what makes that safe.
	if hookBeforeNegotiate != nil {
		hookBeforeNegotiate()
	}
	forwards, probes, err := ports.Negotiate(ctx, resolved.Ports, ports.Prompter{
		In:          r.o.In,
		Out:         r.msg,
		Interactive: r.o.TTY != nil,
	})
	if err != nil {
		return release(err)
	}
	// Idempotent, and the success path closes explicitly before the spawn; this
	// covers every failure between here and there.
	defer func() { _ = probes.Close() }()

	// The record was claimed before the ports were known, so publish them now.
	if err := r.recordPorts(forwards); err != nil {
		return release(err)
	}

	exe, err := r.executable()
	if err != nil {
		return release(err)
	}

	var stdin bytes.Buffer
	if err := supervisor.EncodeParams(&stdin, &supervisor.Params{
		ProjectID:    r.projectID,
		InstanceName: name,
		ConfigHash:   r.hash,
		Config:       resolved,
		Ports:        forwards,
		StateRoot:    r.d.Store.Root,
		Persist:      r.o.Persist,
	}); err != nil {
		return release(err)
	}

	if r.o.Verbose {
		fmt.Fprintf(r.msg, "creating instance %s (log: %s)\n", name, r.d.Store.LogPath(r.projectID))
	}

	// The probes are released in the instant before the spawn so the supervisor
	// can bind the very ports the user just agreed to. The gap is a race the
	// supervisor retries through; holding them any longer would guarantee it
	// lost.
	if err := probes.Close(); err != nil {
		r.abandon(name)
		return false, err
	}

	pid, procStart, err := r.d.Spawn.Spawn(ctx, exe, []string{superviseCommand}, stdin.Bytes(), r.d.Store.LogPath(r.projectID))
	if err != nil {
		// The creating record is published but nothing will ever advance it.
		// Leaving it would make the next pt shell wait out the boot timeout.
		r.abandon(name)
		return false, fmt.Errorf("shell: spawn supervisor: %w", err)
	}

	// Item 15: written immediately, not left for the supervisor's own claim.
	// GC spares an unsupervised record only until the boot timeout, and that
	// grace period is a backstop for a crash between the two writes — not a
	// licence to leave the field unset.
	if err := r.recordSupervisor(name, pid, procStart); err != nil {
		return false, err
	}
	return true, nil
}

// preflightPersist refuses a --persist boot that could only end badly, while
// there is still a terminal to explain it on.
//
// All three answers are about ownership. pt may boot the user's image in place
// and let the guest write to it, but only if it really is a VM of theirs (not a
// cached registry image, whose next pull would silently discard the changes)
// and only if nothing else is already running it — tart allows exactly one.
//
// A tart that cannot be listed is not treated as a refusal: the supervisor will
// fail loudly enough on its own, and refusing to boot because a status command
// hiccuped would be worse than the problem.
func (r *runner) preflightPersist(ctx context.Context, image string) error {
	vms, err := r.d.Tart.List(ctx)
	if err != nil {
		fmt.Fprintf(r.msg, "warning: could not check %s before booting it with --persist: %v\n", image, err)
		return nil
	}
	for _, vm := range vms {
		if vm.Name != image {
			continue
		}
		if vm.Source == tart.SourceOCI {
			return fmt.Errorf("shell: cannot boot %s with --persist: it is a cached remote image, "+
				"and changes written to one are lost on the next pull.\n"+
				"Make a local VM once, then point image: at it:\n"+
				"    tart clone %s my-vm", image, image)
		}
		if vm.State == tart.StateRunning {
			return fmt.Errorf("shell: cannot boot %s with --persist: it is already running.\n"+
				"Only one VM can run an image at a time, so another --persist shell "+
				"(or a plain tart run) has it; exit that one first", image)
		}
		return nil
	}
	return fmt.Errorf("shell: cannot boot %s with --persist: tart has no local VM by that name.\n"+
		"--persist boots the image itself, so it has to be one that exists locally:\n"+
		"    tart clone %s my-vm", image, image)
}

// recordPorts publishes the negotiated forwards onto the record this shell
// already claimed, so pt ports can show them while the VM is still creating.
func (r *runner) recordPorts(forwards []ports.Resolved) error {
	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("shell: the instance record disappeared while ports were being negotiated")
	}
	inst.Ports = portMaps(forwards)
	return r.d.Store.WriteInstance(r.projectID, inst)
}

// claim publishes the creating record, or reports false if the project already
// has one. Both happen in a single lock hold, which is what makes two
// simultaneous first-runs produce one VM rather than two.
//
// vmName is what tart will be told: the clone's name normally, the base image's
// own name under --persist. Recording it here rather than deriving it later is
// what lets garbage collection know which VMs it may delete and which it may
// only stop.
func (r *runner) claim(name, vmName string) (bool, error) {
	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return false, err
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil {
		return false, err
	}
	if inst != nil {
		// Any record at all means the world moved while we were prompting. The
		// caller re-decides rather than guessing which of us owns the project.
		return false, nil
	}
	return true, r.d.Store.WriteInstance(r.projectID, &state.Instance{
		InstanceName: name,
		VMName:       vmName,
		Persist:      r.o.Persist,
		ProjectPath:  r.projectDir,
		ConfigHash:   r.hash,
		State:        state.StateCreating,
		CreatedAt:    r.clk.Now(),
		// Empty rather than absent: the forwards are not negotiated yet, and
		// recordPorts publishes them onto this record moments from now.
		Ports: portMaps(nil),
	})
}

// recordSupervisor stamps the spawned child's identity onto the record.
//
// The supervisor writes the same two fields itself as its first act, and this
// write may land before or after it. Both orders are safe because the value is
// identical — supervisor.claim exempts its own PID — but only if this write
// refuses to touch a record that is no longer ours.
func (r *runner) recordSupervisor(name string, pid int, procStart uint64) error {
	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil {
		return err
	}
	if inst == nil || inst.InstanceName != name {
		return nil
	}
	if inst.SupervisorPID == pid && inst.SupervisorStart != 0 {
		// The supervisor already claimed itself, with start ticks read from
		// inside the process that owns them. Those are the better value.
		return nil
	}
	inst.SupervisorPID, inst.SupervisorStart = pid, procStart
	return r.d.Store.WriteInstance(r.projectID, inst)
}

// abandon removes a creating record this shell published but could not hand to
// a supervisor. It is best effort: the record's PID is unset, so garbage
// collection reclaims it after the boot timeout even if this fails.
func (r *runner) abandon(name string) {
	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil || inst == nil || inst.InstanceName != name || inst.SupervisorPID > 0 {
		return
	}
	_ = r.d.Store.RemoveProject(r.projectID)
}

// register records this shell against the instance, in the same lock hold that
// re-confirms the instance is running.
//
// The two must be atomic together. The supervisor publishes state stopping in
// the same lock hold in which it observes an empty session set (agent G's
// claimIdle), so a registration that merely followed a running read could land
// against a VM already condemned. Re-reading here is the counterpart: whichever
// of the two takes the lock second sees the other's decision.
//
// A nil session with a nil error means the instance was condemned; the caller
// starts over.
func (r *runner) register(want *state.Instance) (*state.Session, error) {
	pid, procStart, err := state.Self()
	if err != nil {
		return nil, fmt.Errorf("shell: identify self: %w", err)
	}

	lk, err := r.d.Store.Lock(r.projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := r.d.Store.ReadInstance(r.projectID)
	if err != nil {
		return nil, err
	}
	if inst == nil || inst.State != state.StateRunning || inst.InstanceName != want.InstanceName {
		return nil, nil
	}

	sess := &state.Session{
		PID:       pid,
		ProcStart: procStart,
		StartedAt: r.clk.Now(),
		TTY:       ttyName(r.o.TTY),
	}
	if err := r.d.Store.AddSession(r.projectID, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// deregister removes this shell's session record. It runs from a defer, so it
// also runs while a panic unwinds and after a signal has cancelled the session.
func (r *runner) deregister(sess *state.Session) {
	if sess == nil {
		return
	}
	lk, err := r.d.Store.Lock(r.projectID)
	if err == nil {
		defer func() { _ = lk.Unlock() }()
		err = r.d.Store.RemoveSession(r.projectID, sess.ID)
	}
	if err != nil {
		// Reported, not returned: the user's exit status belongs to their shell,
		// and this failure is self-healing anyway — the record names this
		// process, which is about to exit, so the supervisor's next poll drops
		// it as a dead PID.
		fmt.Fprintf(r.msg, "warning: could not remove this session's record: %v\n", err)
	}
}

// session dials the guest and hands the terminal over.
func (r *runner) session(ctx context.Context, inst *state.Instance) (int, error) {
	if inst.VMIP == "" {
		return exitTransport, fmt.Errorf("shell: instance %s is running but has no address; see %s",
			inst.InstanceName, r.d.Store.LogPath(r.projectID))
	}
	addr := net.JoinHostPort(inst.VMIP, strconv.Itoa(sshPort))

	// A single attempt, deliberately. Backoff belongs to the boot path, which
	// the supervisor already completed before publishing running; retrying here
	// would only delay the report of a VM that has since died.
	cl, err := sshx.Dial(ctx, addr, r.d.Creds)
	if err != nil {
		return exitTransport, r.sessionFailure(err)
	}
	defer func() { _ = cl.Close() }()

	// The status banner rides along whenever there is a terminal to put it
	// on; sshx quietly declines it for terminals too small to split. Its
	// poll loop lives exactly as long as the session.
	var opts []sshx.InteractiveOption
	if r.o.TTY != nil {
		bn := newBanner(r.cfg.Image, r.networkOpen(), inst.VM(), inst.VMPID, inst.Persist)
		pollCtx, stopPoll := context.WithCancel(ctx)
		defer stopPoll()
		go bn.poll(pollCtx, r.d, r.projectID)
		opts = append(opts, sshx.WithStatusLine(bn.line))
	}

	code, err := cl.Interactive(ctx, sshx.LoginCommand(config.GuestProjectPath()), r.o.TTY, opts...)
	if err != nil {
		return code, r.sessionFailure(err)
	}
	return code, nil
}

// networkOpen reports whether the guest's outbound network is unrestricted,
// which the banner warns about. It reads the current config rather than the
// instance's snapshot — the two differ only across a config drift, where the
// current file is the one the user can act on.
func (r *runner) networkOpen() bool {
	return r.cfg.Network == nil || r.cfg.Network.Policy != config.NetRestricted
}

// sessionFailure explains a transport failure in terms of the VM rather than
// the socket.
//
// The common cause is spec §6.3 step 5: the guest died and the supervisor tore
// it down underneath us. supervisor.log is the only record of why, so the
// message has to name it — an SSH error alone would send the user looking at
// their network.
func (r *runner) sessionFailure(err error) error {
	log := r.d.Store.LogPath(r.projectID)
	inst, readErr := r.readInstance()
	if readErr == nil && (inst == nil || inst.State == state.StateDead || inst.State == state.StateStopping) {
		return fmt.Errorf("VM terminated unexpectedly; see %s: %w", log, err)
	}
	return fmt.Errorf("%w; if this persists, see %s", err, log)
}

// waitForRunning polls the instance record until the supervisor publishes
// running, showing a spinner while it does.
//
// The lock is not held: this can take the whole of ptcfg.BootTimeout, and every
// other pt invocation for the project — including the supervisor's own state
// writes — would be blocked behind it.
func (r *runner) waitForRunning(ctx context.Context) (*state.Instance, error) {
	deadline := r.clk.Now().Add(ptcfg.BootTimeout)
	sp := r.spinner("waiting for VM to boot")
	defer sp.stop()

	tk := r.clk.NewTicker(ptcfg.CreatingPollInterval)
	defer tk.Stop()

	for {
		inst, err := r.readInstance()
		if err != nil {
			return nil, err
		}
		switch {
		case inst == nil:
			// The supervisor tore the instance down and removed its state. It
			// left the log behind, which is the only thing that says why.
			return nil, r.bootFailure("the instance was removed while it was starting")
		case inst.State == state.StateRunning:
			return inst, nil
		case inst.State == state.StateDead:
			// A failed boot deliberately stops here rather than cleaning up:
			// the dead record and the log are the user's whole diagnosis.
			return nil, r.bootFailure("the VM failed to start")
		case inst.SupervisorPID > 0 && !state.Alive(inst.SupervisorPID, inst.SupervisorStart):
			// The supervisor died without recording why. Waiting out the full
			// boot timeout for a process that no longer exists helps nobody.
			return nil, r.bootFailure("the supervisor exited before the VM was ready")
		}

		if !r.clk.Now().Before(deadline) {
			return nil, r.bootFailure(fmt.Sprintf("the VM did not become ready within %s", ptcfg.BootTimeout))
		}
		sp.tick()
		select {
		case <-tk.C():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitForGone waits for an instance that is shutting down to finish, so that
// the fresh one this shell wants is not created into the middle of a teardown.
func (r *runner) waitForGone(ctx context.Context) error {
	deadline := r.clk.Now().Add(ptcfg.StoppingWaitTimeout)
	sp := r.spinner("waiting for the previous VM to shut down")
	defer sp.stop()

	tk := r.clk.NewTicker(ptcfg.CreatingPollInterval)
	defer tk.Stop()

	for {
		inst, err := r.readInstance()
		if err != nil {
			return err
		}
		// A record with no live supervisor is finished, whatever it says: the
		// next decide's GC force-stops the clone and clears the state. Waiting
		// for the state field to reach dead as well would hang on precisely the
		// case where nothing is left to write it.
		if inst == nil || !state.Alive(inst.SupervisorPID, inst.SupervisorStart) {
			return nil
		}
		if !r.clk.Now().Before(deadline) {
			return fmt.Errorf("shell: the previous instance for %s did not shut down within %s; see %s",
				r.projectDir, ptcfg.StoppingWaitTimeout, r.d.Store.LogPath(r.projectID))
		}
		sp.tick()
		select {
		case <-tk.C():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// readInstance reads the record under a shared lock, which is the contract
// state.Store documents for readers.
func (r *runner) readInstance() (*state.Instance, error) {
	lk, err := r.d.Store.RLock(r.projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lk.Unlock() }()
	return r.d.Store.ReadInstance(r.projectID)
}

// bootFailure phrases a failed boot as one sentence pointing at the log.
func (r *runner) bootFailure(what string) error {
	return fmt.Errorf("shell: %s; see %s", what, r.d.Store.LogPath(r.projectID))
}

// executable resolves the pt binary to re-exec as the supervisor.
func (r *runner) executable() (string, error) {
	if r.o.SelfPath != "" {
		return r.o.SelfPath, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("shell: locate the pt binary: %w", err)
	}
	return exe, nil
}

// portMaps converts negotiated forwards into the on-disk shape. The record is
// written before the supervisor exists, so pt ports can already show what this
// instance is going to forward.
func portMaps(forwards []ports.Resolved) []state.PortMap {
	out := make([]state.PortMap, 0, len(forwards))
	for _, f := range forwards {
		out = append(out, state.PortMap{
			VMPort:           f.VMPort,
			HostPort:         f.HostPort,
			OriginalHostPort: f.OriginalHostPort,
		})
	}
	return out
}

// ttyName records which terminal a session is attached to, for pt list. It is
// cosmetic, so an unnamable file is simply omitted.
func ttyName(tty *os.File) string {
	if tty == nil {
		return ""
	}
	return tty.Name()
}
