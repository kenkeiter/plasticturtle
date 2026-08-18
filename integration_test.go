//go:build integration

// Package integration exercises pt against a real Tart VM.
//
// Run with:
//
//	make integration
//
// It is build-tagged off by default because it clones and boots a real macOS
// guest: roughly 30 seconds per boot, and it needs
// ghcr.io/cirruslabs/macos-tahoe-base pulled locally. Everything else in this
// repository is tested against fakes; this file is the only thing that can
// falsify the assumptions those fakes encode.
package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testImage = "ghcr.io/cirruslabs/macos-tahoe-base:latest"

	// bootBudget is generous compared with ptcfg.BootTimeout: a cold host under
	// load can be slower than a warm one, and a flaky integration test teaches
	// people to ignore it.
	bootBudget = 5 * time.Minute
)

// world is one isolated pt installation: its own state root, its own project.
type world struct {
	t       *testing.T
	bin     string
	state   string
	project string
}

func newWorld(t *testing.T, cfg string) *world {
	t.Helper()
	requireTart(t)

	root := t.TempDir()
	w := &world{
		t:       t,
		bin:     buildPT(t),
		state:   filepath.Join(root, "state"),
		project: filepath.Join(root, "project"),
	}
	if err := os.MkdirAll(w.project, 0o755); err != nil {
		t.Fatal(err)
	}
	// The project path must be canonical, because that is the key pt derives
	// its project ID and trust entry from.
	resolved, err := filepath.EvalSymlinks(w.project)
	if err != nil {
		t.Fatal(err)
	}
	w.project = resolved

	if err := os.WriteFile(filepath.Join(w.project, ".plasticturtle"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	w.allow()
	t.Cleanup(w.cleanup)
	return w
}

// cmd runs pt with this world's state root.
func (w *world) cmd(stdin string, args ...string) (string, int) {
	w.t.Helper()
	c := exec.Command(w.bin, args...)
	c.Env = append(os.Environ(), "XDG_STATE_HOME="+w.state)
	c.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out

	code := 0
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			w.t.Fatalf("pt %s: %v\n%s", strings.Join(args, " "), err, out.String())
		}
		code = ee.ExitCode()
	}
	return out.String(), code
}

func (w *world) allow() {
	w.t.Helper()
	out, code := w.cmd("y\n", "allow", w.project)
	if code != 0 {
		w.t.Fatalf("pt allow failed (%d):\n%s", code, out)
	}
}

// shell runs a non-interactive session, feeding script to the guest's shell.
func (w *world) shell(script string) (string, int) {
	w.t.Helper()
	return w.cmd(script, "shell", w.project)
}

// cleanup fails the test if anything was left behind, and then removes it.
//
// A leaked VM is not a cleanup detail: the entire premise of the tool is that
// nothing survives the last shell.
func (w *world) cleanup() {
	w.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.ourVMs()) == 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if leaked := w.ourVMs(); len(leaked) > 0 {
		w.t.Errorf("VMs leaked after the last session exited: %v", leaked)
		for _, name := range leaked {
			_ = exec.Command("tart", "stop", name, "--timeout", "0").Run()
			_ = exec.Command("tart", "delete", name).Run()
		}
	}
	if log := w.supervisorLog(); log != "" && w.t.Failed() {
		w.t.Logf("supervisor.log:\n%s", log)
	}
}

// ourVMs lists the pt-owned VMs belonging to this world's project.
func (w *world) ourVMs() []string {
	out, err := exec.Command("tart", "list", "--format", "json").Output()
	if err != nil {
		return nil
	}
	var vms []struct {
		Name   string `json:"Name"`
		Source string `json:"Source"`
	}
	if err := json.Unmarshal(out, &vms); err != nil {
		return nil
	}
	var names []string
	for _, vm := range vms {
		// Only ever this world's own clones. A test that deletes another pt
		// project's VM would be worse than the bug it was written to catch.
		if strings.EqualFold(vm.Source, "local") && strings.HasPrefix(vm.Name, "pt-") {
			names = append(names, vm.Name)
		}
	}
	return names
}

func (w *world) supervisorLog() string {
	var found string
	_ = filepath.Walk(w.state, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Base(path) == "supervisor.log" {
			if b, err := os.ReadFile(path); err == nil {
				found = string(b)
			}
		}
		return nil
	})
	return found
}

func (w *world) instanceRecords() []string {
	var found []string
	_ = filepath.Walk(w.state, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Base(path) == "instance.json" {
			found = append(found, path)
		}
		return nil
	})
	return found
}

const basicConfig = `version: 1
image: ` + testImage + `
`

// TestEndToEnd is the whole premise of the tool in one test: a project
// directory becomes a booted VM with that directory mounted read-write, and
// nothing survives the session.
func TestEndToEnd(t *testing.T) {
	w := newWorld(t, basicConfig)

	if err := os.WriteFile(filepath.Join(w.project, "from-host.txt"), []byte("host wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := strings.Join([]string{
		"pwd",
		`echo "MARKER:$PT_IN_VM_SESSION"`,
		"cat from-host.txt",
		`echo "guest wrote this" > from-guest.txt`,
		"exit 0",
	}, "\n") + "\n"

	out, code := w.shell(script)
	if code != 0 {
		t.Fatalf("pt shell exited %d:\n%s\n\nsupervisor.log:\n%s", code, out, w.supervisorLog())
	}

	// The login preamble must land the user in the project share, not $HOME.
	if !strings.Contains(out, "/Volumes/My Shared Files/project") {
		t.Errorf("session did not start in the project share:\n%s", out)
	}
	// Nested tooling detects the sandbox through this.
	if !strings.Contains(out, "MARKER:1") {
		t.Errorf("PT_IN_VM_SESSION was not set in the session:\n%s", out)
	}
	// The host's files are visible in the guest...
	if !strings.Contains(out, "host wrote this") {
		t.Errorf("the project directory was not readable in the guest:\n%s", out)
	}
	// ...and the guest's writes reach the host, which is what makes the VM
	// useful rather than merely isolated.
	guestFile := filepath.Join(w.project, "from-guest.txt")
	if b, err := os.ReadFile(guestFile); err != nil {
		t.Errorf("the guest's write did not reach the host: %v", err)
	} else if !strings.Contains(string(b), "guest wrote this") {
		t.Errorf("unexpected content from the guest: %q", string(b))
	}

	// Ephemerality: the clone and the instance record are both gone.
	//
	// Both conditions are waited on together. Teardown deletes the clone and
	// only then removes the state directory, and the gap between the two is
	// about a second in practice — asserting the record immediately after
	// seeing the VM disappear fails on the product doing exactly the right
	// thing, slightly later.
	waitFor(t, 90*time.Second, "the clone and its state to be reclaimed", func() bool {
		return len(w.ourVMs()) == 0 && len(w.instanceRecords()) == 0
	})
}

// TestExitCodeIsMirrored covers spec section 9: pt shell exits with the remote
// shell's status, so a script driving pt can tell whether the work succeeded.
func TestExitCodeIsMirrored(t *testing.T) {
	w := newWorld(t, basicConfig)
	out, code := w.shell("exit 42\n")
	if code != 42 {
		t.Errorf("pt shell exited %d, want 42:\n%s", code, out)
	}
}

// TestPortForwarding proves a guest service is reachable on the host loopback
// address, which no test against a fake can establish.
func TestPortForwarding(t *testing.T) {
	const cfg = `version: 1
image: ` + testImage + `
ports:
  - vm_port: 3000
`
	w := newWorld(t, cfg)

	// Serve exactly one line inside the guest, then let the session end.
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.shell("(printf 'hello-from-guest\\n' | nc -l 3000) &\nsleep 60\n")
	}()

	// Wait for the forward to be reported up before dialing it.
	waitFor(t, bootBudget, "the forward to come up", func() bool {
		out, _ := w.cmd("", "ports", w.project)
		return strings.Contains(out, "forwarding")
	})

	var got string
	waitFor(t, 30*time.Second, "the guest service to answer", func() bool {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:3000", 3*time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return false
		}
		got = line
		return true
	})
	if !strings.Contains(got, "hello-from-guest") {
		t.Errorf("read %q through the forward, want the guest's greeting", got)
	}
	<-done
}

// TestConcurrentShellsShareOneInstance covers the design's shared-instance
// rule: a second pt shell must attach, not boot a second VM.
func TestConcurrentShellsShareOneInstance(t *testing.T) {
	w := newWorld(t, basicConfig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.shell("sleep 45\n")
	}()

	waitFor(t, bootBudget, "the first instance to run", func() bool {
		out, _ := w.cmd("", "list")
		return strings.Contains(out, "running")
	})
	first := w.ourVMs()
	if len(first) != 1 {
		t.Fatalf("expected exactly one VM after the first shell, got %v", first)
	}

	out, code := w.shell("echo second-shell-ran\n")
	if code != 0 {
		t.Fatalf("second shell exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "second-shell-ran") {
		t.Errorf("second shell produced no output:\n%s", out)
	}

	if second := w.ourVMs(); len(second) != 1 || second[0] != first[0] {
		t.Errorf("second shell did not share the instance: before %v, after %v", first, second)
	}
	<-done
}

// TestRecoversFromKilledSupervisor covers the crash-safety backstop in spec
// section 6.3: a supervisor that dies without cleaning up leaves an orphaned VM
// and a stale record, and the next pt shell must reclaim both.
func TestRecoversFromKilledSupervisor(t *testing.T) {
	w := newWorld(t, basicConfig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.shell("sleep 90\n")
	}()

	var pid int
	var orphan string
	waitFor(t, bootBudget, "the instance to run", func() bool {
		recs := w.instanceRecords()
		if len(recs) == 0 {
			return false
		}
		b, err := os.ReadFile(recs[0])
		if err != nil {
			return false
		}
		var rec struct {
			InstanceName  string `json:"instanceName"`
			State         string `json:"state"`
			SupervisorPID int    `json:"supervisorPid"`
		}
		if err := json.Unmarshal(b, &rec); err != nil || rec.State != "running" {
			return false
		}
		pid, orphan = rec.SupervisorPID, rec.InstanceName
		return pid > 0
	})

	// SIGKILL: the supervisor gets no chance to tear anything down, which is
	// precisely the situation the deferred-deletion backstop exists for.
	if err := exec.Command("kill", "-9", fmt.Sprint(pid)).Run(); err != nil {
		t.Fatalf("kill -9 %d: %v", pid, err)
	}

	// Snapshot immediately. The orphan is what makes this test meaningful, and
	// it does not necessarily survive until the session ends: the exiting shell
	// garbage-collects too, so waiting first can reclaim it before we look.
	orphaned := false
	for _, vm := range w.ourVMs() {
		if vm == orphan {
			orphaned = true
		}
	}
	if !orphaned {
		t.Fatalf("VM %s vanished the instant its supervisor was killed; "+
			"a SIGKILLed supervisor is supposed to leave it running for the next pt shell to reclaim", orphan)
	}
	<-done

	out, code := w.shell("echo recovered\n")
	if code != 0 {
		t.Fatalf("recovery shell exited %d:\n%s\n\nsupervisor.log:\n%s", code, out, w.supervisorLog())
	}
	if !strings.Contains(out, "recovered") {
		t.Errorf("recovery shell produced no output:\n%s", out)
	}
	for _, vm := range w.ourVMs() {
		if vm == orphan {
			t.Errorf("the orphaned VM %s was not reclaimed", orphan)
		}
	}
}

// TestUntrustedConfigRefusesToBoot is the security property, end to end: an
// edited config must not boot anything until it is re-allowed.
func TestUntrustedConfigRefusesToBoot(t *testing.T) {
	w := newWorld(t, basicConfig)

	// Exactly what an LLM agent editing the repo would do.
	path := filepath.Join(w.project, ".plasticturtle")
	if err := os.WriteFile(path, []byte(basicConfig+"# appended by something else\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := w.shell("echo should-not-run\n")
	if code == 0 {
		t.Errorf("pt shell booted an unallowed config:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Errorf("the guest ran despite the config being untrusted:\n%s", out)
	}
	if vms := w.ourVMs(); len(vms) != 0 {
		t.Errorf("an untrusted config created VMs: %v", vms)
	}
}

func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}

func requireTart(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tart"); err != nil {
		t.Skip("tart is not installed")
	}
	out, err := exec.Command("tart", "list", "--format", "json").Output()
	if err != nil {
		t.Skipf("tart list failed: %v", err)
	}
	if !strings.Contains(string(out), "macos-tahoe-base") {
		t.Skipf("%s is not pulled; run: tart pull %s", testImage, testImage)
	}
}

// buildPT builds the binary under test once per run.
func buildPT(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pt-integration")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "pt")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/pt")
		if out, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("build pt: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// errorsAs is errors.As, wrapped so the import list stays honest about why the
// errors package is here: exec.ExitError is the only thing this file unwraps.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
