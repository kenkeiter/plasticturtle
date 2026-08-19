package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

// TestHappyPath is the whole lifecycle: a real SSH dial to an in-process
// server, a real forward carrying real bytes, and the state sequence the design
// document specifies, all in microseconds of wall clock.
func TestHappyPath(t *testing.T) {
	guestPort := echoServer(t)
	hostPort := freePort(t)

	h := newHarness(t, ports.Resolved{VMPort: guestPort, HostPort: hostPort})
	h.writeCreating()
	h.start()
	h.waitRunning()

	inst := h.instanceRecord()
	if inst.VMIP != "127.0.0.1" {
		t.Errorf("vmIp = %q, want 127.0.0.1", inst.VMIP)
	}
	if inst.SupervisorPID != os.Getpid() {
		t.Errorf("supervisorPid = %d, want %d", inst.SupervisorPID, os.Getpid())
	}
	if inst.SupervisorStart == 0 {
		t.Error("supervisorStart was not recorded; PID reuse would go undetected")
	}
	want := []state.PortMap{{VMPort: guestPort, HostPort: hostPort}}
	if !reflect.DeepEqual(inst.Ports, want) {
		t.Errorf("ports = %+v, want %+v", inst.Ports, want)
	}

	// Item 12: the flag is honored, not forced, so the supervisor must set it.
	if !h.tc.runOpts().NoGraphics {
		t.Error("tart run was invoked without NoGraphics; every VM would open a window")
	}
	if got := len(h.tc.runOpts().Dirs); got != 2 {
		t.Errorf("tart run got %d dir shares, want 2", got)
	}
	if d := h.tc.runOpts().Dirs[1]; !d.ReadOnly {
		t.Errorf("mount %q should have been read-only", d.Name)
	}

	// The forward is real: this dial crosses the host listener, the SSH
	// connection and the "guest" service.
	assertForwarded(t, hostPort)

	// The heartbeat is fresh the moment the instance is published.
	age, err := h.store.HeartbeatAge(h.projectID, time.Now())
	if err != nil {
		t.Fatalf("HeartbeatAge: %v", err)
	}
	if age > ptcfg.HeartbeatStaleAfter {
		t.Errorf("heartbeat age %s exceeds the stale threshold", age)
	}

	h.addSession("s1")
	h.tick(ptcfg.SessionPollInterval)
	if !h.running() {
		t.Fatal("supervisor tore down while a session was attached")
	}

	h.removeSession("s1")
	h.tick(ptcfg.SessionPollInterval) // observes the empty set, arms the debounce
	if !h.running() {
		t.Fatal("supervisor tore down before the debounce elapsed")
	}
	h.clk.Advance(ptcfg.SessionEmptyDebounce)

	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.log.states(); !reflect.DeepEqual(got, []string{"creating", "running", "stopping", "dead"}) {
		t.Errorf("state sequence = %v, want creating running stopping dead", got)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want just the seed image", got)
	}
	if h.projectDirExists() {
		t.Error("state directory survived a clean teardown")
	}
	if n := h.tc.n("Delete"); n != 1 {
		t.Errorf("Delete called %d times, want 1", n)
	}
	// The host listener must be gone with the tunnel.
	if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err != nil {
		t.Errorf("host port %d is still bound after teardown: %v", hostPort, err)
	} else {
		_ = ln.Close()
	}
}

// TestHeartbeatKeepsBeating covers the beat that happens on the interval rather
// than at publish time.
func TestHeartbeatKeepsBeating(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	path := h.store.HeartbeatPath(h.projectID)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove heartbeat: %v", err)
	}
	h.clk.Advance(ptcfg.HeartbeatInterval)
	eventually(t, "the heartbeat file to be rewritten", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})

	h.cancel()
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestSessionReturnsWithinDebounce is the reason the debounce exists: `exit &&
// pt shell` must not destroy the VM it is re-entering.
func TestSessionReturnsWithinDebounce(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	h.tick(ptcfg.SessionPollInterval) // empty set observed, debounce armed
	h.addSession("returning")
	h.tick(ptcfg.SessionEmptyDebounce) // the debounce expires on a live session

	if !h.running() {
		t.Fatal("supervisor tore down even though a session returned within the debounce")
	}
	if got := h.instanceState(); got != state.StateRunning {
		t.Fatalf("state = %q, want running", got)
	}

	// And once it really leaves, teardown proceeds.
	h.removeSession("returning")
	h.tick(ptcfg.SessionPollInterval)
	if !h.running() {
		t.Fatal("teardown fired before the debounce")
	}
	h.clk.Advance(ptcfg.SessionEmptyDebounce)

	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.projectDirExists() {
		t.Error("state directory survived a clean teardown")
	}
}

// TestVMDiesUnexpectedly covers spec §6.3 step 5. The state files are kept on
// purpose: supervisor.log is the only explanation the user will get, and the
// record in state dead is what a waiting pt shell reads.
func TestVMDiesUnexpectedly(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()
	h.addSession("attached")

	h.fake.Process(h.instance).Exit(errors.New("hypervisor fault"))

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil after the VM died")
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
	if !h.projectDirExists() {
		t.Error("state directory was removed; supervisor.log is the only diagnosis available")
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want just the seed image", got)
	}
}

// TestSIGTERMDuringRunning covers spec §6.3 step 7.
func TestSIGTERMDuringRunning(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	// Safe because Run has signal.Notify installed for the whole of this
	// window: the runtime hands SIGTERM to the supervisor's channel instead of
	// killing the test binary.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.log.states(); !reflect.DeepEqual(got, []string{"creating", "running", "stopping", "dead"}) {
		t.Errorf("state sequence = %v, want creating running stopping dead", got)
	}
	if h.projectDirExists() {
		t.Error("state directory survived a signalled teardown")
	}
}

// TestSIGTERMDuringBoot: "tear down now" must be answerable during the two
// minutes a boot may take, which is when a user is most likely to send it.
func TestSIGTERMDuringBoot(t *testing.T) {
	h := newHarness(t)
	h.fake.SetIP(h.instance, "") // the guest never gets a lease
	h.writeCreating()
	h.start()

	// Boot deadline + heartbeat + the IP poll's wait: the boot is under way.
	h.clk.BlockUntil(3)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil after being signalled mid-boot")
	}
	if errors.Is(err, errBootTimeout) {
		t.Errorf("error = %v; the signal must not be reported as a boot timeout", err)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after an abandoned boot = %v, want the clone cleaned up", got)
	}
}

// TestContextCancelTearsDown is the API-level equivalent of SIGTERM.
func TestContextCancelTearsDown(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	h.cancel()

	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want just the seed image", got)
	}
}

// TestBootTimeoutWithoutIP drives the 120-second boot bound with a fake clock.
func TestBootTimeoutWithoutIP(t *testing.T) {
	h := newHarness(t)
	// No SetIP for this instance: tart answers ErrNoIP forever.
	h.fake.SetIP(h.instance, "")
	h.writeCreating()
	h.start()

	// Boot deadline + heartbeat + the IP poll's own wait.
	h.clk.BlockUntil(3)
	h.clk.Advance(ptcfg.BootTimeout)

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil after the boot timed out")
	}
	if !errors.Is(err, errBootTimeout) {
		t.Errorf("error = %v, want it to wrap the boot timeout", err)
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
	if !h.projectDirExists() {
		t.Error("state directory was removed; the waiting shell has nothing to report")
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after a failed boot = %v, want the clone deleted", got)
	}
	if got := h.log.states(); !reflect.DeepEqual(got, []string{"creating", "dead"}) {
		t.Errorf("state sequence = %v, want creating dead", got)
	}
}

// TestCloneFailure is the boot failure that happens before there is anything
// to clean up.
func TestCloneFailure(t *testing.T) {
	h := newHarness(t)
	h.fake.FailNext("Clone", errors.New("no space left on device"))
	h.writeCreating()
	h.start()

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil after the clone failed")
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
	if n := h.tc.n("Delete"); n != 0 {
		t.Errorf("Delete called %d times for a clone that was never made", n)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms = %v, want just the seed image", got)
	}
}

// TestVMDiesDuringBoot must not wait out the full boot timeout: a `tart run`
// that fails immediately is a failed boot, not a slow one.
func TestVMDiesDuringBoot(t *testing.T) {
	h := newHarness(t)
	h.fake.SetIP(h.instance, "")
	h.writeCreating()
	h.start()

	h.clk.BlockUntil(3)
	h.fake.Process(h.instance).Exit(errors.New("tart run: exit status 2"))

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil after the VM died during boot")
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
}

// TestResourceOverridesSkippedWhenZero keeps a pointless `tart set` out of the
// boot path, where every subprocess is a second of the user's wait.
func TestResourceOverridesSkippedWhenZero(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()
	if n := h.tc.n("Set"); n != 0 {
		t.Errorf("Set called %d times with no overrides configured", n)
	}
	h.cancel()
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestResourceOverridesApplied is the other half of that decision.
func TestResourceOverridesApplied(t *testing.T) {
	h := newHarness(t)
	h.params.Config.CPU = 4
	h.params.Config.Memory = 8192
	h.writeCreating()
	h.start()
	h.waitRunning()
	if n := h.tc.n("Set"); n != 1 {
		t.Errorf("Set called %d times, want 1", n)
	}
	h.cancel()
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestMissingMountFailsBeforeCloning is item 2's defense in depth: the shell
// checked these paths, but the supervisor is what hands them to tart.
func TestMissingMountFailsBeforeCloning(t *testing.T) {
	h := newHarness(t)
	h.params.Config.Mounts[1].HostPath = h.projectDir + "/definitely-not-here"
	h.writeCreating()
	h.start()

	err := h.finish()
	if err == nil {
		t.Fatal("Run returned nil for a mount whose host path does not exist")
	}
	if n := h.tc.n("Clone"); n != 0 {
		t.Errorf("Clone was called %d times despite the invalid mount", n)
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
}

// TestTeardownFromTwoWatchersAtOnce is the property this package exists to get
// right: the session debounce and the dying VM both reach for teardown in the
// same instant, and it happens once.
func TestTeardownFromTwoWatchersAtOnce(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	h.tick(ptcfg.SessionPollInterval) // empty set observed, debounce armed

	var start sync.WaitGroup
	var racers sync.WaitGroup
	start.Add(1)
	racers.Add(2)
	go func() {
		defer racers.Done()
		start.Wait()
		h.fake.Process(h.instance).Exit(errors.New("hypervisor fault"))
	}()
	go func() {
		defer racers.Done()
		start.Wait()
		h.clk.Advance(ptcfg.SessionEmptyDebounce)
	}()
	start.Done()
	racers.Wait()

	_ = h.finish()

	if n := len(h.log.withPrefix("teardown: ")); n != 1 {
		t.Errorf("teardown ran %d times, want exactly 1", n)
	}
	if n := h.tc.n("Delete"); n != 1 {
		t.Errorf("Delete called %d times, want exactly 1", n)
	}
	deads := 0
	for _, s := range h.log.states() {
		if s == string(state.StateDead) {
			deads++
		}
	}
	if deads != 1 {
		t.Errorf("state dead written %d times, want exactly 1", deads)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want just the seed image", got)
	}
}

// TestPersistBootsTheImageAndLeavesIt is --persist's whole contract: nothing is
// cloned on the way in, tart is asked about the image by its own name, and the
// image is still there — stopped, not deleted — afterwards.
func TestPersistBootsTheImageAndLeavesIt(t *testing.T) {
	h := newHarness(t).persist()
	h.writeCreating()
	h.start()
	h.waitRunning()

	if n := h.tc.n("Clone"); n != 0 {
		t.Errorf("Clone called %d times for a --persist instance", n)
	}
	inst := h.instanceRecord()
	if !inst.Persist {
		t.Error("the record does not say the instance is persistent; GC would delete the image")
	}
	if inst.VMName != baseImage {
		t.Errorf("vmName = %q, want the image %q", inst.VMName, baseImage)
	}
	if inst.VM() != baseImage {
		t.Errorf("VM() = %q, want %q", inst.VM(), baseImage)
	}
	if inst.InstanceName != h.instance {
		t.Errorf("instanceName = %q, want the generated name %q", inst.InstanceName, h.instance)
	}

	h.tick(ptcfg.SessionPollInterval)
	h.clk.Advance(ptcfg.SessionEmptyDebounce)
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := h.tc.n("Delete"); n != 0 {
		t.Fatalf("Delete called %d times: --persist destroyed the user's image", n)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want the image intact", got)
	}
	if h.projectDirExists() {
		t.Error("state directory survived a clean teardown of a persistent instance")
	}
}

// TestPersistStopsGracefullyFirst matters more here than for a clone: this VM's
// disk is the user's, and a forced stop is a power cut on a filesystem they
// expect to still be intact tomorrow.
func TestPersistStopsGracefullyFirst(t *testing.T) {
	h := newHarness(t).persist()
	h.writeCreating()
	h.start()
	h.waitRunning()

	h.tick(ptcfg.SessionPollInterval)
	h.clk.Advance(ptcfg.SessionEmptyDebounce)
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := h.tc.n("StopForce"); n != 0 {
		t.Errorf("the image was force-stopped %d times during a clean teardown", n)
	}
	if n := h.tc.n("Stop"); n != 1 {
		t.Errorf("Stop called %d times, want 1", n)
	}
}

// assertForwarded proves a tunnel carries bytes end to end.
func assertForwarded(t *testing.T, hostPort int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)), 10*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	const payload = "turtle"
	if _, err := fmt.Fprint(conn, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != payload {
		t.Errorf("forwarded %q, want %q", buf, payload)
	}
}
