//go:build darwin

package vmnetlink

import (
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestOpenRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; the euid check cannot fail")
	}
	_, err := Open(Config{Subnet: netip.MustParsePrefix("192.168.249.0/24")})
	if err == nil {
		t.Fatal("Open succeeded without root")
	}
}

func TestOpenValidatesBeforePrivilegeCheck(t *testing.T) {
	// A bad subnet must be named as such whether or not the caller is root,
	// otherwise "run me as root" masks a config error.
	_, err := Open(Config{Subnet: netip.MustParsePrefix("192.168.249.1/24")})
	if err == nil {
		t.Fatal("Open accepted a non-base subnet")
	}
	if !strings.Contains(err.Error(), "network base") {
		t.Fatalf("error %q does not mention the subnet problem", err)
	}
}

// TestLiveLink exercises the real framework. It needs root and leaves a
// bridge interface behind for its duration, so it is opt-in:
//
//	sudo PT_VMNETLINK_LIVE=1 go test ./internal/vmnetlink/ -run Live -v
func TestLiveLink(t *testing.T) {
	if os.Getenv("PT_VMNETLINK_LIVE") != "1" {
		t.Skip("set PT_VMNETLINK_LIVE=1 (and run as root) to exercise vmnet")
	}
	if os.Geteuid() != 0 {
		t.Fatal("PT_VMNETLINK_LIVE=1 requires root")
	}

	subnet := netip.MustParsePrefix("192.168.249.0/24")
	l, err := Open(Config{Subnet: subnet, Isolation: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if want := netip.MustParseAddr("192.168.249.1"); l.Gateway() != want {
		t.Errorf("Gateway() = %v, want %v", l.Gateway(), want)
	}
	if l.MaxPacketSize() <= 0 {
		t.Errorf("MaxPacketSize() = %d, want > 0", l.MaxPacketSize())
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := l.ReadFrame(make([]byte, l.MaxPacketSize())); err == nil {
		t.Error("ReadFrame after Close returned no error")
	}
}

// TestLiveCloseUnblocksReader covers the shim's shutdown path: Close must wake a
// reader that is parked waiting for a frame that may never come.
func TestLiveCloseUnblocksReader(t *testing.T) {
	if os.Getenv("PT_VMNETLINK_LIVE") != "1" {
		t.Skip("set PT_VMNETLINK_LIVE=1 (and run as root) to exercise vmnet")
	}
	if os.Geteuid() != 0 {
		t.Fatal("PT_VMNETLINK_LIVE=1 requires root")
	}

	l, err := Open(Config{Subnet: netip.MustParsePrefix("192.168.249.0/24"), Isolation: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, l.MaxPacketSize())
		for {
			if _, err := l.ReadFrame(buf); err != nil {
				done <- err
				return
			}
		}
	}()

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("blocked ReadFrame returned no error after Close")
	}
}
