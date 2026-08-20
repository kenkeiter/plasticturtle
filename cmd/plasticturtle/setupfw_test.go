package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/state"
)

func TestLocateShimBinary(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "custom-shim")
	if err := os.WriteFile(shim, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := locateShimBinary(shim)
	if err != nil || got != shim {
		t.Fatalf("locateShimBinary(explicit) = (%q, %v), want %q", got, err, shim)
	}
	if _, err := locateShimBinary("/definitely/missing"); err == nil {
		// An explicit-but-missing path falls through to the other candidates;
		// with none present it must error.
		if _, err2 := os.Stat(filepath.Join(filepath.Dir(os.Args[0]), "plasticturtle-softnet-shim")); err2 != nil {
			t.Fatal("expected error when no shim candidate exists")
		}
	}
}

func TestCopyFilePreservesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("shim-bytes")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("copied content = %q, want %q (err %v)", got, want, err)
	}
	fi, _ := os.Stat(dst)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("copied mode = %o, want 0755", fi.Mode().Perm())
	}
}

// TestRunSetupFirewallInstallsAndEscalates checks the install steps up to (but
// not including) the privilege the test cannot grant: the shim is copied into
// place and the privileged command is the chown-then-chmod, in that order.
func TestRunSetupFirewallInstallsAndEscalates(t *testing.T) {
	root := t.TempDir()
	store := &state.Store{Root: root}
	src := filepath.Join(t.TempDir(), "plasticturtle-softnet-shim")
	if err := os.WriteFile(src, []byte("shim"), 0o755); err != nil {
		t.Fatal(err)
	}

	var privArgs []string
	priv := func(argv ...string) error { privArgs = argv; return nil }

	var out bytes.Buffer
	// verifyShim will fail (we cannot chown root in a test), so the call returns
	// an error — but only after copying and escalating, which is what we assert.
	_ = runSetupFirewall(store, src, &out, priv)

	if _, err := os.Stat(store.ShimPath()); err != nil {
		t.Fatalf("shim was not installed at %s: %v", store.ShimPath(), err)
	}
	if len(privArgs) < 3 || privArgs[0] != "sh" || privArgs[1] != "-c" {
		t.Fatalf("privileged command = %v, want sh -c ...", privArgs)
	}
	script := privArgs[2]
	chown := strings.Index(script, "chown root")
	chmod := strings.Index(script, "chmod u+s")
	if chown < 0 || chmod < 0 {
		t.Fatalf("privileged script missing chown/chmod: %q", script)
	}
	if chown > chmod {
		t.Errorf("chmod u+s must come after chown (chown clears setuid): %q", script)
	}
	// Only the shim is rooted: there is no external softnet binary any more.
	if !strings.Contains(script, store.ShimPath()) {
		t.Errorf("privileged script does not target the shim: %q", script)
	}
	if strings.Contains(script, "/bin/softnet") {
		t.Errorf("privileged script still roots an external softnet: %q", script)
	}

	// The guidance must not send users to Homebrew or com.apple.vmnet.plist;
	// pt pins the sandbox subnet itself.
	for _, stale := range []string{"brew", "com.apple.vmnet", "bootpd"} {
		if strings.Contains(out.String(), stale) {
			t.Errorf("setup output still mentions %q:\n%s", stale, out.String())
		}
	}
}
