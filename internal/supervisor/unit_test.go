package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// validParams is a parameter set that passes validation, for tests that mutate
// one field at a time.
func validParams(t *testing.T) *Params {
	t.Helper()
	dir := t.TempDir()
	projectID := state.ProjectID(dir)
	name, err := state.NewInstanceName(projectID)
	if err != nil {
		t.Fatalf("NewInstanceName: %v", err)
	}
	return &Params{
		ProjectID:    projectID,
		InstanceName: name,
		ConfigHash:   "sha256:" + strings.Repeat("b", 64),
		StateRoot:    dir,
		Ports:        []ports.Resolved{{VMPort: 3000, HostPort: 13000, OriginalHostPort: 3000}},
		Config: &config.Resolved{
			ProjectPath: dir,
			Image:       "ghcr.io/example/image:latest",
			Mounts:      []config.ResolvedMount{{Name: "project", HostPath: dir, Mode: config.ModeRW}},
		},
	}
}

func TestParamsRoundTrip(t *testing.T) {
	want := validParams(t)

	var buf bytes.Buffer
	if err := EncodeParams(&buf, want); err != nil {
		t.Fatalf("EncodeParams: %v", err)
	}
	// The whole reason for stdin: a mount path or a port must never be visible
	// in another user's ps output, so it may not be argv.
	if !strings.Contains(buf.String(), want.Config.ProjectPath) {
		t.Fatalf("encoded params do not carry the project path: %s", buf.String())
	}

	got, err := ParseParams(&buf)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestParseParamsRejectsIncompleteInput(t *testing.T) {
	base := validParams(t)

	tests := []struct {
		name    string
		mutate  func(*Params)
		rawJSON string
		want    string
	}{
		{name: "empty input", rawJSON: "", want: "no parameters"},
		{name: "not json", rawJSON: "{", want: "decode parameters"},
		{
			name:    "unknown field",
			rawJSON: `{"projectId":"0123456789abcdef","secretPlan":true}`,
			want:    "decode parameters",
		},
		{
			name:    "trailing data",
			rawJSON: `{"projectId":"0123456789abcdef"} {"projectId":"0123456789abcdef"}`,
			want:    "trailing data",
		},
		{name: "no project id", mutate: func(p *Params) { p.ProjectID = "" }, want: "malformed project id"},
		{name: "short project id", mutate: func(p *Params) { p.ProjectID = "abc" }, want: "malformed project id"},
		{name: "no instance name", mutate: func(p *Params) { p.InstanceName = "" }, want: "malformed instance name"},
		{
			name:   "instance name from another project",
			mutate: func(p *Params) { p.InstanceName = "pt-" + strings.Repeat("f", 16) + "-deadbeef" },
			want:   "does not belong to project",
		},
		{name: "no config hash", mutate: func(p *Params) { p.ConfigHash = " " }, want: "missing config hash"},
		{name: "relative state root", mutate: func(p *Params) { p.StateRoot = "state" }, want: "not absolute"},
		{name: "no config", mutate: func(p *Params) { p.Config = nil }, want: "missing config snapshot"},
		{name: "no image", mutate: func(p *Params) { p.Config.Image = "" }, want: "names no image"},
		{
			name:   "relative project path",
			mutate: func(p *Params) { p.Config.ProjectPath = "./project" },
			want:   "not absolute",
		},
		{
			name:   "relative mount path",
			mutate: func(p *Params) { p.Config.Mounts[0].HostPath = "data" },
			want:   "is not absolute",
		},
		{
			name:   "unnamed mount",
			mutate: func(p *Params) { p.Config.Mounts[0].Name = "" },
			want:   "has no name",
		},
		{
			name:   "port out of range",
			mutate: func(p *Params) { p.Ports[0].VMPort = 0 },
			want:   "vm port 0 out of range",
		},
		{
			name: "duplicate host port",
			mutate: func(p *Params) {
				p.Ports = append(p.Ports, ports.Resolved{VMPort: 5432, HostPort: p.Ports[0].HostPort})
			},
			want: "repeats host port",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.rawJSON
			if tc.mutate != nil {
				p := *base
				cfg := *base.Config
				cfg.Mounts = append([]config.ResolvedMount(nil), base.Config.Mounts...)
				p.Config = &cfg
				p.Ports = append([]ports.Resolved(nil), base.Ports...)
				tc.mutate(&p)

				// Encode with the validating path bypassed: these are exactly the
				// messages a malformed writer would produce.
				var buf bytes.Buffer
				if err := writeJSON(&buf, &p); err != nil {
					t.Fatalf("encode: %v", err)
				}
				raw = buf.String()
			}
			_, err := ParseParams(strings.NewReader(raw))
			if err == nil {
				t.Fatalf("ParseParams(%q) returned no error", raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestEncodeParamsRejectsInvalid(t *testing.T) {
	if err := EncodeParams(&bytes.Buffer{}, nil); err == nil {
		t.Error("EncodeParams(nil) returned no error")
	}
	p := validParams(t)
	p.ProjectID = "nope"
	if err := EncodeParams(&bytes.Buffer{}, p); err == nil {
		t.Error("EncodeParams accepted a malformed project id; the shell would never see the complaint")
	}
}

// writeJSON encodes without validating, so that ParseParams' own checks — not
// EncodeParams' — are what the table above exercises.
func writeJSON(w *bytes.Buffer, p *Params) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func TestRunRejectsMissingDeps(t *testing.T) {
	p := validParams(t)
	if err := Run(context.Background(), p, Deps{}); err == nil {
		t.Error("Run with no tart client returned no error")
	}
	if err := Run(context.Background(), nil, Deps{}); err == nil {
		t.Error("Run with no params returned no error")
	}
}

// TestChooseRemoteAddr covers item 17: the guest-side dial target is decided
// once, at setup, not per connection.
func TestGuestAddrs(t *testing.T) {
	tests := []struct {
		name string
		vmIP string
		want []string
	}{
		{
			name: "loopback first, guest address as fallback",
			vmIP: "192.168.64.7",
			want: []string{"127.0.0.1:3000", "192.168.64.7:3000"},
		},
		{
			// Nothing to fall back to: the two candidates would be identical.
			name: "loopback only when the guest address is loopback",
			vmIP: "127.0.0.1",
			want: []string{"127.0.0.1:3000"},
		},
		{
			name: "loopback only when the guest address is unknown",
			vmIP: "",
			want: []string{"127.0.0.1:3000"},
		},
		{
			name: "loopback only when the guest address is unparseable",
			vmIP: "not-an-ip",
			want: []string{"127.0.0.1:3000"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := guestAddrs(tc.vmIP, 3000)
			if len(got) != len(tc.want) {
				t.Fatalf("guestAddrs(%q) = %v, want %v", tc.vmIP, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("guestAddrs(%q)[%d] = %q, want %q", tc.vmIP, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRequestStopIsSingleWinner is the primitive teardown's idempotence rests
// on: many watchers, one reason, one close.
func TestRequestStopIsSingleWinner(t *testing.T) {
	r := &run{stopped: make(chan struct{}), childDone: make(chan struct{})}

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.requestStop(stopKind(i%4), "reason %d", i)
		}(i)
	}
	wg.Wait()

	select {
	case <-r.stopped:
	default:
		t.Fatal("requestStop did not wake the watchers")
	}
	if !strings.HasPrefix(r.stopReason, "reason ") {
		t.Errorf("stopReason = %q, want one of the racing reasons", r.stopReason)
	}
}

// TestTeardownIsIdempotent enters the shutdown sequence from many goroutines at
// once — the situation every watcher plus a signal can genuinely produce — and
// asserts the VM is stopped and deleted exactly once.
func TestTeardownIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	projectID := state.ProjectID(dir)
	instance, err := state.NewInstanceName(projectID)
	if err != nil {
		t.Fatalf("NewInstanceName: %v", err)
	}

	fake := tart.NewFake(baseImage)
	if err := fake.Clone(context.Background(), baseImage, instance); err != nil {
		t.Fatalf("clone: %v", err)
	}
	tc := newCountingTart(fake)
	lg := &testLog{}

	r := &run{
		p: &Params{
			ProjectID:    projectID,
			InstanceName: instance,
			ConfigHash:   "sha256:x",
			StateRoot:    store.Root,
			Config:       &config.Resolved{ProjectPath: dir, Image: baseImage},
		},
		d:          Deps{Tart: tc, Store: store, Clock: sys.NewFakeClock(time.Now()), Logf: lg.logf},
		clk:        sys.NewFakeClock(time.Now()),
		stopped:    make(chan struct{}),
		childDone:  make(chan struct{}),
		cloned:     true,
		booted:     true,
		published:  true,
		stopKind:   stopIdle,
		stopReason: "no sessions remain",
	}
	// The child is already gone, so the graceful stop needs no timer.
	close(r.childDone)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.teardown(context.Background())
		}()
	}
	wg.Wait()

	if n := tc.n("Stop"); n != 1 {
		t.Errorf("Stop called %d times, want 1", n)
	}
	if n := tc.n("Delete"); n != 1 {
		t.Errorf("Delete called %d times, want 1", n)
	}
	if n := len(lg.withPrefix("teardown: ")); n != 1 {
		t.Errorf("teardown announced %d times, want exactly 1", n)
	}
	if got := lg.states(); !reflect.DeepEqual(got, []string{"stopping", "dead"}) {
		t.Errorf("state sequence = %v, want stopping dead", got)
	}
	if got := fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms after teardown = %v, want just the seed image", got)
	}
}

// TestClaimIdleIsAtomic is the narrow race the debounce cannot cover: a pt
// shell that takes the lock between the last poll and the decision must not be
// left attached to a VM that is already being destroyed.
func TestClaimIdleIsAtomic(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	r := &run{p: h.params, d: h.deps, clk: h.clk, stopped: make(chan struct{}), childDone: make(chan struct{})}

	h.addSession("attached")
	if r.claimIdle() {
		t.Fatal("claimed an instance that still has a live session")
	}
	if got := h.instanceState(); got != state.StateCreating {
		t.Errorf("state = %q, want the record left alone", got)
	}

	h.removeSession("attached")
	if !r.claimIdle() {
		t.Fatal("failed to claim an idle instance")
	}
	if got := h.instanceState(); got != state.StateStopping {
		t.Errorf("state = %q, want stopping published under the same lock", got)
	}
	if !r.stopping.Load() {
		t.Error("teardown was not told that stopping is already published")
	}
}

// TestStopKindDecidesStateRemoval documents the one thing the reason changes.
func TestStopKindDecidesStateRemoval(t *testing.T) {
	tests := map[stopKind]bool{
		stopIdle:       true,
		stopSignal:     true,
		stopVMDied:     false,
		stopBootFailed: false,
	}
	for kind, want := range tests {
		if got := kind.removesState(); got != want {
			t.Errorf("stopKind(%d).removesState() = %v, want %v", kind, got, want)
		}
	}
}

// TestForwardBindFailureFailsBoot: a host port that cannot be bound must not be
// advertised as forwarding.
func TestForwardBindFailureFailsBoot(t *testing.T) {
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = blocked.Close() }()

	h := newHarness(t, ports.Resolved{VMPort: 3000, HostPort: portOf(t, blocked.Addr().String())})
	h.writeCreating()
	h.start()

	// The single rebind retry waits one backoff interval before giving up.
	h.clk.BlockUntil(3)
	h.clk.Advance(time.Second)

	runErr := h.finish()
	if runErr == nil {
		t.Fatal("Run returned nil for a host port that could not be bound")
	}
	if !strings.Contains(runErr.Error(), "forward host port") {
		t.Errorf("error = %v, want it to name the port", runErr)
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
	if got := h.fake.Existing(); !reflect.DeepEqual(got, []string{baseImage}) {
		t.Errorf("vms = %v, want the clone cleaned up", got)
	}
}

// TestClaimRefusesAnotherInstance stops a second supervisor from overwriting a
// record that names a VM it does not own — which would orphan that VM.
func TestClaimRefusesAnotherInstance(t *testing.T) {
	h := newHarness(t)
	other, err := state.NewInstanceName(h.projectID)
	if err != nil {
		t.Fatalf("NewInstanceName: %v", err)
	}

	lk, err := h.store.Lock(h.projectID)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	writeErr := h.store.WriteInstance(h.projectID, &state.Instance{
		InstanceName: other,
		ProjectPath:  h.projectDir,
		State:        state.StateRunning,
		CreatedAt:    h.clk.Now(),
	})
	_ = lk.Unlock()
	if writeErr != nil {
		t.Fatalf("write instance: %v", writeErr)
	}

	h.start()
	runErr := h.finish()
	if runErr == nil || !strings.Contains(runErr.Error(), "already has instance") {
		t.Fatalf("Run error = %v, want a refusal to supervise", runErr)
	}
	if n := h.tc.n("Clone"); n != 0 {
		t.Errorf("Clone called %d times before the refusal", n)
	}
	if got := h.instanceRecord().InstanceName; got != other {
		t.Errorf("instance record was overwritten with %q", got)
	}
}
