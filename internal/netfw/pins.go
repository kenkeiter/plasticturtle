package netfw

import (
	"net/netip"
	"sort"
	"time"
)

// maxPins bounds the pin table so a guest cannot exhaust host memory by
// resolving an unbounded stream of allowed names. When full, the soonest-to-
// expire entry is evicted to make room — it is the pin with the least remaining
// value. 8192 distinct live destinations is far past any real workload.
const maxPins = 8192

// PinTable is the set of destination IPs the guest is currently allowed to
// reach, each with an expiry. It is populated from allowed DNS answers and read
// on every non-DNS frame, so both paths are hot; access is guarded by the
// Filter's mutex rather than here, keeping this type a plain data structure.
type PinTable struct {
	pins map[netip.Addr]time.Time // ip -> expiry
}

// NewPinTable returns an empty table.
func NewPinTable() *PinTable {
	return &PinTable{pins: make(map[netip.Addr]time.Time)}
}

// Pin records that ip is reachable until expiry, extending any existing pin
// whose expiry is sooner. A later resolution of the same name thus refreshes the
// window rather than shortening it.
func (t *PinTable) Pin(ip netip.Addr, expiry time.Time) {
	ip = ip.Unmap()
	if cur, ok := t.pins[ip]; ok {
		if expiry.After(cur) {
			t.pins[ip] = expiry
		}
		return
	}
	if len(t.pins) >= maxPins {
		t.evictSoonest()
	}
	t.pins[ip] = expiry
}

// Allowed reports whether ip is pinned and unexpired as of now. Expired entries
// are dropped lazily on lookup, which is enough to keep the table from growing
// without a background sweeper.
func (t *PinTable) Allowed(ip netip.Addr, now time.Time) bool {
	ip = ip.Unmap()
	exp, ok := t.pins[ip]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(t.pins, ip)
		return false
	}
	return true
}

// Len reports the number of entries, expired or not. Intended for tests and
// metrics, not verdicts.
func (t *PinTable) Len() int { return len(t.pins) }

// evictSoonest removes the entry with the earliest expiry. Called only when the
// table is at capacity, so the O(n) scan is rare and bounded by maxPins.
func (t *PinTable) evictSoonest() {
	var victim netip.Addr
	var best time.Time
	first := true
	for ip, exp := range t.pins {
		if first || exp.Before(best) {
			victim, best, first = ip, exp, false
		}
	}
	if !first {
		delete(t.pins, victim)
	}
}

// snapshot returns the live pins sorted by IP, for tests and debug logging.
func (t *PinTable) snapshot(now time.Time) []netip.Addr {
	out := make([]netip.Addr, 0, len(t.pins))
	for ip, exp := range t.pins {
		if now.Before(exp) {
			out = append(out, ip)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}
