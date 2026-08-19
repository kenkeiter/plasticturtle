package supervisor

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

func TestDetectSubnetCollision(t *testing.T) {
	guest := netip.MustParseAddr("192.168.2.2")
	tests := []struct {
		name   string
		ifaces []hostIface
		want   bool
	}{
		{
			name: "lan collides with guest subnet",
			ifaces: []hostIface{
				{name: "en0", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.2.223/24")}},
				{name: "bridge100", isVMNet: true, prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.2.1/24")}},
			},
			want: true,
		},
		{
			name: "distinct lan does not collide",
			ifaces: []hostIface{
				{name: "en0", prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.1.50/24")}},
				{name: "bridge100", isVMNet: true, prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.2.1/24")}},
			},
			want: false,
		},
		{
			name: "only the vmnet bridge is on the subnet (healthy)",
			ifaces: []hostIface{
				{name: "en0", prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.1.50/24")}},
				{name: "bridge100", isVMNet: true, prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.2.1/24")}},
			},
			want: false,
		},
		{
			name: "wider host supernet overlaps guest",
			ifaces: []hostIface{
				{name: "utun3", prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/16")}},
			},
			want: true,
		},
		{
			name:   "ipv6 host address is ignored",
			ifaces: []hostIface{{name: "en0", prefixes: []netip.Prefix{netip.MustParsePrefix("fe80::1/64")}}},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := detectSubnetCollision(guest, tt.ifaces)
			if got != tt.want {
				t.Fatalf("detectSubnetCollision = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOpenPolicyLeavesNetworkingUntouched is the regression guard: a project
// without a restricted policy must boot exactly as before — no --net-softnet,
// no injected environment.
func TestOpenPolicyLeavesNetworkingUntouched(t *testing.T) {
	h := newHarness(t)
	h.writeCreating()
	h.start()
	h.waitRunning()

	opts := h.tc.runOpts()
	if opts.Softnet {
		t.Error("open policy set Softnet; NAT behavior must be unchanged")
	}
	if len(opts.Env) != 0 {
		t.Errorf("open policy injected env %v, want none", opts.Env)
	}

	// Tear down cleanly: SIGTERM is delivered to the supervisor's own handler
	// for the duration of Run.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if err := h.finish(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRestrictedWithoutShimFailsBoot proves the firewall fails closed: a
// restricted policy with no installed shim aborts the boot with an actionable
// error rather than silently booting with open networking.
func TestRestrictedWithoutShimFailsBoot(t *testing.T) {
	h := newHarness(t)
	h.params.Config.Network = config.ResolvedNetwork{
		Policy: config.NetRestricted,
		Allow:  []string{"github.com"},
	}
	h.writeCreating()
	h.start()

	err := h.finish()
	if err == nil {
		t.Fatal("restricted boot succeeded with no shim installed; must fail closed")
	}
	if !strings.Contains(err.Error(), "firewall shim") {
		t.Errorf("error = %v, want it to mention the firewall shim", err)
	}
	if got := h.instanceState(); got != state.StateDead {
		t.Errorf("state = %q, want dead", got)
	}
	// The clone must not have been booted through tart run.
	if n := h.tc.n("Run"); n != 0 {
		t.Errorf("tart run called %d times for a boot that should abort before it", n)
	}
}

func TestVerifyShimRejectsMissingAndUnsafe(t *testing.T) {
	// Missing.
	if err := verifyShim("/no/such/shim", os.Stat); err == nil {
		t.Fatal("missing shim should be rejected")
	}
	// Present but not root-owned / not setuid (a normal temp file). This is the
	// common misconfiguration: built but setup not run.
	dir := t.TempDir()
	p := filepath.Join(dir, "softnet")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifyShim(p, os.Stat); err == nil {
		t.Fatal("a non-root, non-setuid shim should be rejected")
	}
}
