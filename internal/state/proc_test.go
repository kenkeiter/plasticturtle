package state

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSelfRoundTrip(t *testing.T) {
	pid, start, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("Self pid = %d, want %d", pid, os.Getpid())
	}
	if start == 0 {
		t.Fatal("Self start = 0; a recorded identity with no start time cannot guard against PID reuse")
	}
	if !Alive(pid, start) {
		t.Fatal("Alive(self) = false")
	}
}

// TestAliveRejectsStartMismatch is the reason this mechanism exists: the PID is
// real and running, but it is not the process the record was written about.
func TestAliveRejectsStartMismatch(t *testing.T) {
	pid, start, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	for _, fabricated := range []uint64{start + 1, start - 1, 1, ^uint64(0)} {
		if fabricated == start {
			continue
		}
		if Alive(pid, fabricated) {
			t.Fatalf("Alive(%d, %d) = true; PID reuse would go undetected", pid, fabricated)
		}
	}
}

func TestAliveFalseForReapedChild(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	pid := cmd.Process.Pid

	// Existence-only check (start == 0) is the weaker fallback and is what a
	// legacy record would use; both forms must report dead.
	if Alive(pid, 0) {
		t.Fatalf("Alive(%d, 0) = true for a reaped child", pid)
	}
	if Alive(pid, 12345) {
		t.Fatalf("Alive(%d, 12345) = true for a reaped child", pid)
	}
}

func TestAliveRejectsNonPositivePIDs(t *testing.T) {
	// PID 0 is the kernel task and answers KERN_PROC_PID, so the guard has to
	// be explicit: an unset supervisorPid must never read as alive.
	for _, pid := range []int{0, -1, -100} {
		if Alive(pid, 0) {
			t.Fatalf("Alive(%d, 0) = true", pid)
		}
		if _, err := ProcStart(pid); err == nil {
			t.Fatalf("ProcStart(%d) succeeded", pid)
		}
	}
}

func TestProcStartUnknownPID(t *testing.T) {
	// A PID above the kernel's maximum can never exist.
	if _, err := ProcStart(1 << 30); err == nil {
		t.Fatal("ProcStart(2^30) succeeded")
	}
}

func TestProcStartStableAcrossCalls(t *testing.T) {
	a, err := ProcStart(os.Getpid())
	if err != nil {
		t.Fatalf("ProcStart: %v", err)
	}
	b, err := ProcStart(os.Getpid())
	if err != nil {
		t.Fatalf("ProcStart: %v", err)
	}
	if a != b {
		t.Fatalf("ProcStart is not stable: %d then %d", a, b)
	}
}

func TestTreeStats(t *testing.T) {
	stats, err := TreeStats(os.Getpid())
	if err != nil {
		t.Fatalf("TreeStats: %v", err)
	}
	if stats.RSSBytes == 0 {
		t.Fatal("TreeStats reported zero RSS for the test process")
	}
	if stats.CPUPercent < 0 {
		t.Fatalf("TreeStats reported negative CPU: %v", stats.CPUPercent)
	}

	// The tree must include descendants, so a subtree rooted at launchd cannot
	// be smaller than this process alone.
	all, err := TreeStats(1)
	if err != nil {
		t.Fatalf("TreeStats(1): %v", err)
	}
	if all.RSSBytes < stats.RSSBytes {
		t.Fatalf("tree of pid 1 (%d bytes) smaller than this process alone (%d bytes)", all.RSSBytes, stats.RSSBytes)
	}

	if _, err := TreeStats(1 << 30); err == nil {
		t.Fatal("TreeStats on a nonexistent pid succeeded")
	}
}

func TestDiskUsageBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, 64*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := DiskUsageBytes(dir)
	if err != nil {
		t.Fatalf("DiskUsageBytes: %v", err)
	}
	if n < 64*1024 {
		t.Fatalf("DiskUsageBytes = %d, want at least 65536", n)
	}

	// A VM directory that does not exist yet is zero, not an error: pt list
	// renders a row for an instance whose clone is still being made.
	n, err = DiskUsageBytes(filepath.Join(dir, "missing"))
	if err != nil || n != 0 {
		t.Fatalf("DiskUsageBytes(missing) = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := DiskUsageBytes(""); err == nil {
		t.Fatal("DiskUsageBytes(\"\") succeeded")
	}
}

func TestTartVMDir(t *testing.T) {
	t.Setenv("TART_HOME", "/opt/tarthome")
	got, err := TartVMDir("pt-0123456789abcdef-01234567")
	if err != nil {
		t.Fatalf("TartVMDir: %v", err)
	}
	if want := "/opt/tarthome/vms/pt-0123456789abcdef-01234567"; got != want {
		t.Fatalf("TartVMDir = %q, want %q", got, want)
	}

	t.Setenv("TART_HOME", "")
	got, err = TartVMDir("vm")
	if err != nil {
		t.Fatalf("TartVMDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if want := filepath.Join(home, ".tart", "vms", "vm"); got != want {
		t.Fatalf("TartVMDir = %q, want %q", got, want)
	}
	if _, err := TartVMDir(""); err == nil {
		t.Fatal("TartVMDir(\"\") succeeded")
	}
}
