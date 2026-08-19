package netfw

import (
	"net/netip"
	"testing"
	"time"
)

func TestPinTableExpiry(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	pt := NewPinTable()
	ip := netip.MustParseAddr("1.2.3.4")
	pt.Pin(ip, base.Add(time.Minute))

	if !pt.Allowed(ip, base) {
		t.Fatal("pin should be live at base")
	}
	if !pt.Allowed(ip, base.Add(59*time.Second)) {
		t.Fatal("pin should be live before expiry")
	}
	if pt.Allowed(ip, base.Add(time.Minute)) {
		t.Fatal("pin should be dead at expiry (half-open interval)")
	}
	if pt.Len() != 0 {
		t.Fatal("expired pin should be dropped lazily on lookup")
	}
}

func TestPinTableExtendNeverShortens(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	pt := NewPinTable()
	ip := netip.MustParseAddr("1.2.3.4")
	pt.Pin(ip, base.Add(5*time.Minute))
	pt.Pin(ip, base.Add(time.Minute)) // shorter; must not win
	if !pt.Allowed(ip, base.Add(4*time.Minute)) {
		t.Fatal("a shorter re-pin must not shorten an existing pin")
	}
}

func TestPinTableEviction(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	pt := NewPinTable()
	// Fill past capacity; the soonest-to-expire should be evicted.
	for i := range maxPins {
		ip := netip.AddrFrom4([4]byte{byte(i >> 16), byte(i >> 8), byte(i), 1})
		pt.Pin(ip, base.Add(time.Duration(i+10)*time.Second))
	}
	soonest := netip.AddrFrom4([4]byte{0, 0, 0, 1}) // expiry base+10s, the minimum
	overflow := netip.MustParseAddr("9.9.9.9")
	pt.Pin(overflow, base.Add(time.Hour))
	if pt.Allowed(soonest, base) {
		t.Fatal("soonest-to-expire pin should have been evicted at capacity")
	}
	if !pt.Allowed(overflow, base) {
		t.Fatal("newly added pin should be present")
	}
}
