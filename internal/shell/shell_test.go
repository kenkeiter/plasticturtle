package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/supervisor"
)

// A config that was never allowed must not reach tart, and must not produce a
// prompt: this is the security choke point, and a prompt here would train users
// to approve files they have not read.
func TestRunRejectsUntrustedConfig(t *testing.T) {
	h := newBareHarness(t, testConfig)

	code, err := h.run()
	if err == nil {
		t.Fatal("expected an error for an untrusted config")
	}
	if err.Error() != untrustedMessage {
		t.Errorf("error = %q, want the spec §5 wording %q", err, untrustedMessage)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Errorf("spawned %d supervisors, want none", len(calls))
	}
	if inst := h.instance(); inst != nil {
		t.Errorf("wrote an instance record %+v, want none", inst)
	}
}

// A config allowed at different bytes is the same hard error. Nothing about the
// message distinguishes "changed" from "never allowed", by design.
func TestRunRejectsChangedConfig(t *testing.T) {
	h := newBareHarness(t, testConfig)
	h.allow("sha256:" + strings.Repeat("a", 64))

	code, err := h.run()
	if err == nil || err.Error() != untrustedMessage {
		t.Fatalf("error = %v, want %q", err, untrustedMessage)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Errorf("spawned %d supervisors, want none", len(calls))
	}
}

// A directory that is not part of a project reports config.ErrNotFound, which
// the CLI layer distinguishes from a malformed config.
func TestRunReportsMissingConfig(t *testing.T) {
	h := newBareHarness(t, "")

	code, err := h.run()
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap config.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), config.ErrNotFound.Error()) {
		t.Errorf("error = %q, want it to contain %q", err, config.ErrNotFound)
	}
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
}

// The create path is the whole contract with the supervisor: it is spawned as
// this binary's hidden subcommand, with its parameters on stdin and its stdio
// pointed at the project's log.
func TestRunCreatePathSpawnsSupervisorWithResolvedParams(t *testing.T) {
	h := newHarness(t)

	code, err := h.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	calls := h.spawn.recorded()
	if len(calls) != 1 {
		t.Fatalf("spawned %d supervisors, want 1", len(calls))
	}
	call := calls[0]
	if call.exe != "/usr/local/bin/pt" {
		t.Errorf("executable = %q, want the pt binary", call.exe)
	}
	if len(call.args) != 1 || call.args[0] != superviseCommand {
		t.Errorf("args = %q, want [%q]", call.args, superviseCommand)
	}
	if want := h.store.LogPath(h.projectID); call.log != want {
		t.Errorf("log path = %q, want %q", call.log, want)
	}

	// The round trip the real supervisor performs on its own stdin.
	p, err := supervisor.ParseParams(bytes.NewReader(call.stdin))
	if err != nil {
		t.Fatalf("parse params: %v", err)
	}
	if p.ProjectID != h.projectID {
		t.Errorf("project id = %q, want %q", p.ProjectID, h.projectID)
	}
	if p.ConfigHash != h.hash {
		t.Errorf("config hash = %q, want %q", p.ConfigHash, h.hash)
	}
	if p.StateRoot != h.store.Root {
		t.Errorf("state root = %q, want %q", p.StateRoot, h.store.Root)
	}
	if !state.InstanceNamePattern.MatchString(p.InstanceName) {
		t.Errorf("instance name %q does not match the pattern GC is allowed to reclaim", p.InstanceName)
	}
	if p.Config.Image != baseImage {
		t.Errorf("image = %q, want %q", p.Config.Image, baseImage)
	}
	if p.Config.ProjectPath != h.dir {
		t.Errorf("project path = %q, want the canonical %q", p.Config.ProjectPath, h.dir)
	}
	if len(p.Config.Mounts) != 1 {
		t.Fatalf("mounts = %+v, want just the implicit project share", p.Config.Mounts)
	}
	if m := p.Config.Mounts[0]; m.Name != config.ProjectMountName || m.HostPath != h.dir {
		t.Errorf("project mount = %+v, want %s -> %s", m, config.ProjectMountName, h.dir)
	}
	if len(p.Ports) != 1 || p.Ports[0].VMPort != 3000 || p.Ports[0].HostPort <= 0 {
		t.Errorf("ports = %+v, want one forward from vm port 3000", p.Ports)
	}

	// Plan item 15: the shell records the child's identity as soon as the spawn
	// returns, rather than leaving GC's boot-timeout grace period to cover it.
	inst := h.instance()
	if inst == nil {
		t.Fatal("no instance record after a successful create")
	}
	if inst.InstanceName != p.InstanceName {
		t.Errorf("record names %q, supervisor was told %q", inst.InstanceName, p.InstanceName)
	}
	if inst.SupervisorPID != h.spawn.pid || inst.SupervisorStart != h.spawn.start {
		t.Errorf("supervisor identity = (%d, %d), want (%d, %d)",
			inst.SupervisorPID, inst.SupervisorStart, h.spawn.pid, h.spawn.start)
	}
}

// A second pt shell for a live instance shares it. Spawning a second supervisor
// would mean two VMs for one project.
func TestRunAttachesToRunningInstanceWithoutSpawning(t *testing.T) {
	h := newHarness(t)
	want := h.runningInstance(h.hash)

	code, err := h.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Fatalf("spawned %d supervisors, want none", len(calls))
	}
	if got := h.instance(); got == nil || got.InstanceName != want.InstanceName {
		t.Errorf("instance = %+v, want the pre-existing %q", got, want.InstanceName)
	}
	if strings.Contains(h.err.String(), configDriftNote) {
		t.Errorf("printed the drift note for an unchanged config: %q", h.err.String())
	}
}

// A running VM keeps the image and mounts it booted with, so an edited config
// is reported rather than applied.
func TestRunPrintsConfigDriftNote(t *testing.T) {
	h := newHarness(t)
	h.runningInstance("sha256:" + strings.Repeat("b", 64))

	if _, err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.err.String(); !strings.Contains(got, configDriftNote) {
		t.Errorf("output = %q, want it to contain %q", got, configDriftNote)
	}
}

// A shell arriving while another is booting waits for running rather than
// creating a second VM.
func TestRunWaitsForCreatingInstance(t *testing.T) {
	h := newHarness(t)
	pid, start := h.liveSupervisor()
	name := h.instanceName()
	h.writeInstance(&state.Instance{
		InstanceName:    name,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateCreating,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       h.clk.Now(),
	})

	done := h.start()
	// One waiter: the poll ticker. Reaching it means the shell is parked in the
	// boot wait, which is when the world may change underneath it.
	h.clk.BlockUntil(1)

	h.writeInstance(&state.Instance{
		InstanceName:    name,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateRunning,
		SupervisorPID:   pid,
		SupervisorStart: start,
		VMIP:            "127.0.0.1",
		CreatedAt:       h.clk.Now(),
	})
	h.clk.Advance(ptcfg.CreatingPollInterval)

	res := h.await(done)
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	if res.code != 0 {
		t.Errorf("exit code = %d, want 0", res.code)
	}
	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Errorf("spawned %d supervisors, want none", len(calls))
	}
}

// A boot that fails leaves the record dead and the log in place. That log is
// the user's only diagnosis, so the error has to name it.
func TestRunReportsFailedBoot(t *testing.T) {
	h := newHarness(t)
	pid, start := h.liveSupervisor()
	name := h.instanceName()
	h.writeInstance(&state.Instance{
		InstanceName:    name,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateCreating,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       h.clk.Now(),
	})

	done := h.start()
	h.clk.BlockUntil(1)

	h.writeInstance(&state.Instance{
		InstanceName:    name,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateDead,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       h.clk.Now(),
	})
	h.clk.Advance(ptcfg.CreatingPollInterval)

	res := h.await(done)
	if res.err == nil {
		t.Fatal("expected an error for an instance that reached dead")
	}
	if !strings.Contains(res.err.Error(), h.store.LogPath(h.projectID)) {
		t.Errorf("error = %q, want it to name %q", res.err, h.store.LogPath(h.projectID))
	}
	if res.code != exitFailure {
		t.Errorf("exit code = %d, want %d", res.code, exitFailure)
	}
	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Errorf("spawned %d supervisors, want none", len(calls))
	}
}

// Creating into the middle of a teardown would race the tart delete that is
// about to happen, so a stopping instance is waited out first.
func TestRunWaitsForStoppingInstanceThenCreatesFresh(t *testing.T) {
	h := newHarness(t)
	pid, start := h.liveSupervisor()
	previous := h.instanceName()
	h.writeInstance(&state.Instance{
		InstanceName:    previous,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateStopping,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       h.clk.Now(),
	})

	done := h.start()
	h.clk.BlockUntil(1) // the shutdown wait's poll ticker

	if calls := h.spawn.recorded(); len(calls) != 0 {
		t.Fatalf("spawned a supervisor while the previous instance was still stopping")
	}

	// Teardown completes: the supervisor removes the project's state.
	h.removeInstance()
	h.clk.Advance(ptcfg.CreatingPollInterval)

	res := h.await(done)
	if res.err != nil {
		t.Fatalf("run: %v", res.err)
	}
	calls := h.spawn.recorded()
	if len(calls) != 1 {
		t.Fatalf("spawned %d supervisors, want 1", len(calls))
	}
	p, err := supervisor.ParseParams(bytes.NewReader(calls[0].stdin))
	if err != nil {
		t.Fatalf("parse params: %v", err)
	}
	if p.InstanceName == previous {
		t.Errorf("reused the previous instance name %q, want a fresh one", previous)
	}
}

// A supervisor that died mid-boot leaves a creating record naming a VM nobody
// owns. GC reclaims both under the lock, and this shell then starts over.
func TestRunRecoversFromDeadSupervisorDuringCreating(t *testing.T) {
	h := newHarness(t)
	abandoned := h.instanceName()
	if err := h.vms.Clone(context.Background(), baseImage, abandoned); err != nil {
		t.Fatalf("seed abandoned vm: %v", err)
	}
	pid, start := h.deadSupervisor()
	h.writeInstance(&state.Instance{
		InstanceName:    abandoned,
		ProjectPath:     h.dir,
		ConfigHash:      h.hash,
		State:           state.StateCreating,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       h.clk.Now(),
	})

	code, err := h.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	for _, name := range h.vms.Existing() {
		if name == abandoned {
			t.Errorf("left the abandoned clone %q behind", abandoned)
		}
	}
	calls := h.spawn.recorded()
	if len(calls) != 1 {
		t.Fatalf("spawned %d supervisors, want 1", len(calls))
	}
	p, err := supervisor.ParseParams(bytes.NewReader(calls[0].stdin))
	if err != nil {
		t.Fatalf("parse params: %v", err)
	}
	if p.InstanceName == abandoned {
		t.Errorf("reused the abandoned instance name %q", abandoned)
	}
}

// The session record is what keeps the supervisor from tearing the VM down. It
// must exist for exactly as long as the SSH session does.
func TestRunRegistersSessionAndRemovesItOnExit(t *testing.T) {
	h := newHarness(t)
	h.runningInstance(h.hash)

	observed := make(chan int, 1)
	commands := make(chan string, 1)
	h.guest.SetExecHandler(func(cmd string, _ io.Reader, _, _ io.Writer) int {
		sessions, err := h.store.ListSessions(h.projectID)
		if err != nil {
			observed <- -1
		} else {
			observed <- len(sessions)
		}
		commands <- cmd
		return 0
	})

	if _, err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := <-observed; got != 1 {
		t.Errorf("%d sessions registered during the session, want 1", got)
	}
	if cmd := <-commands; !strings.Contains(cmd, "PT_IN_VM_SESSION=1") {
		t.Errorf("login command = %q, want the pt preamble", cmd)
	}
	if got := h.sessionCount(); got != 0 {
		t.Errorf("%d session records after exit, want 0", got)
	}
}

// The failure that matters most: a session record left behind pins the VM open
// until garbage collection notices, so it must be removed even when the session
// never happened.
func TestRunRemovesSessionWhenSSHFails(t *testing.T) {
	h := newHarness(t)
	h.runningInstance(h.hash)

	// Port 1 on loopback refuses immediately: the instance is advertised as
	// running, but nothing answers where its sshd should be.
	sshPort = 1

	code, err := h.run()
	if err == nil {
		t.Fatal("expected an error when the guest cannot be reached")
	}
	if code != exitTransport {
		t.Errorf("exit code = %d, want %d for a transport failure", code, exitTransport)
	}
	if !strings.Contains(err.Error(), h.store.LogPath(h.projectID)) {
		t.Errorf("error = %q, want it to name the supervisor log", err)
	}
	if got := h.sessionCount(); got != 0 {
		t.Fatalf("%d session records after a failed session, want 0", got)
	}
}

// The same path a SIGINT or SIGTERM takes: signal.NotifyContext cancels the
// context, the session closes, and the deferred deregistration still runs. A
// record left behind here would pin the VM open until GC noticed.
func TestRunRemovesSessionWhenCancelled(t *testing.T) {
	h := newHarness(t)
	h.runningInstance(h.hash)

	entered := make(chan struct{})
	release := make(chan struct{})
	// Registered after the guest's own cleanup and therefore run before it, so
	// the server's Close is never left waiting on a blocked handler.
	t.Cleanup(func() { close(release) })
	h.guest.SetExecHandler(func(string, io.Reader, io.Writer, io.Writer) int {
		close(entered)
		<-release
		return 0
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() {
		code, err := Run(ctx, h.opts(), h.deps())
		done <- result{code: code, err: err}
	}()

	<-entered
	if got := h.sessionCount(); got != 1 {
		t.Fatalf("%d sessions registered during the session, want 1", got)
	}
	cancel()

	res := h.await(done)
	if res.err == nil {
		t.Error("expected an error for a cancelled session")
	}
	if got := h.sessionCount(); got != 0 {
		t.Errorf("%d session records after cancellation, want 0", got)
	}
}

// pt shell's exit status is the remote shell's, so that scripts and shell
// chains behave as if the command had run locally.
func TestRunMirrorsRemoteExitStatus(t *testing.T) {
	h := newHarness(t)
	h.runningInstance(h.hash)
	h.guest.SetExecHandler(func(string, io.Reader, io.Writer, io.Writer) int { return 42 })

	code, err := h.run()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 42 {
		t.Errorf("exit code = %d, want the remote shell's 42", code)
	}
	if got := h.sessionCount(); got != 0 {
		t.Errorf("%d session records after exit, want 0", got)
	}
}
