package netfw

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// TTL bounds for pins derived from DNS answers. A record's own TTL is honored
// within these limits: the floor keeps a hostile TTL of 0 from making a name
// unreachable the instant after it resolves, and the ceiling keeps a very long
// TTL from pinning a since-moved CDN address for hours.
const (
	minPinTTL = 30 * time.Second
	maxPinTTL = 10 * time.Minute
)

// Action is the verdict for one frame.
type Action int

const (
	// Pass forwards the frame unchanged.
	Pass Action = iota
	// Drop discards the frame.
	Drop
	// Replace forwards ReplacementFrame instead of the original.
	Replace
)

// Verdict is what Filter returns for a frame: an action and, for Replace, the
// substitute bytes. A short Reason is attached to denials for logging.
type Verdict struct {
	Action           Action
	ReplacementFrame []byte
	Reason           string
}

var (
	pass = Verdict{Action: Pass}
	drop = Verdict{Action: Drop}
)

// Filter applies a domain policy to a guest's frames by DNS-pinning. It is safe
// for concurrent use: the two relay directions call EgressFromGuest and
// IngressToGuest from separate goroutines, and both touch the pin table.
type Filter struct {
	matcher *Matcher
	now     func() time.Time

	mu   sync.Mutex
	pins *PinTable

	// onDeny, if set, is called (without the lock held) with a short reason each
	// time a frame is dropped or rewritten. The shim uses it to log the domain a
	// user likely needs to add to their allowlist.
	onDeny func(reason string)
}

// Config configures a Filter.
type Config struct {
	// Allow is the normalized allowlist (config.ResolvedNetwork.Allow).
	Allow []string
	// Now overrides the clock; nil means time.Now. Tests inject a fake.
	Now func() time.Time
	// OnDeny, if set, receives a one-line reason for each denied flow.
	OnDeny func(reason string)
}

// New builds a Filter for a restricted policy. A Filter is only meaningful under
// NetRestricted; the open policy runs no filter at all (the shim relays raw).
func New(cfg Config) *Filter {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Filter{
		matcher: NewMatcher(cfg.Allow),
		now:     now,
		pins:    NewPinTable(),
		onDeny:  cfg.OnDeny,
	}
}

// EgressFromGuest rules on a frame the guest is sending outward. The default is
// Drop: only frames the policy positively permits are passed.
//
//   - ARP is passed (link-layer address resolution, not egress).
//   - IPv6 is dropped: pins are IPv4-only, so passing IPv6 would be an
//     unfiltered path around the whole policy.
//   - DNS to any resolver is passed so the guest can resolve names; the answers
//     are what IngressToGuest pins on.
//   - DHCP is passed so the guest keeps its lease.
//   - Any other IPv4 frame is passed only if its destination is pinned.
func (f *Filter) EgressFromGuest(frame []byte) Verdict {
	switch etherType(frame) {
	case ethTypeARP:
		return pass
	case ethTypeIPv4:
		// handled below
	default:
		return f.deny("non-IPv4 egress (%s) dropped", ethTypeName(frame))
	}
	v, ok := parseIPv4(frame)
	if !ok {
		return f.deny("undecodable IPv4 egress dropped")
	}
	if sp, dp, ok := v.l4Ports(); ok {
		if dp == dnsPort {
			return pass // outbound query; pinning happens on the reply
		}
		if isDHCP(sp, dp) {
			return pass
		}
	}
	f.mu.Lock()
	allowed := f.pins.Allowed(v.dst, f.now())
	f.mu.Unlock()
	if allowed {
		return pass
	}
	return f.deny("blocked egress to %s (no allowed DNS answer pinned it)", v.dst)
}

// IngressToGuest rules on a frame arriving for the guest. It is where allowed
// DNS answers are pinned and disallowed ones are rewritten to NXDOMAIN.
//
//   - ARP and DHCP replies are passed.
//   - A DNS response for an allowed name has its A records pinned, then passes.
//   - A DNS response for a disallowed name is replaced with NXDOMAIN.
//   - Any other IPv4 frame passes only if its source is already pinned (it is
//     return traffic for a connection egress permitted).
func (f *Filter) IngressToGuest(frame []byte) Verdict {
	switch etherType(frame) {
	case ethTypeARP:
		return pass
	case ethTypeIPv4:
		// handled below
	default:
		return f.deny("non-IPv4 ingress (%s) dropped", ethTypeName(frame))
	}
	v, ok := parseIPv4(frame)
	if !ok {
		return f.deny("undecodable IPv4 ingress dropped")
	}
	if sp, dp, ok := v.l4Ports(); ok {
		if isDHCP(sp, dp) {
			return pass
		}
		if sp == dnsPort {
			return f.handleDNSResponse(v)
		}
	}
	f.mu.Lock()
	allowed := f.pins.Allowed(v.src, f.now())
	f.mu.Unlock()
	if allowed {
		return pass
	}
	return f.deny("dropped ingress from unpinned %s", v.src)
}

// handleDNSResponse pins allowed answers or rewrites disallowed ones. A response
// whose UDP payload cannot be parsed is passed through unchanged: it may be a
// legitimate reply the parser simply does not model, and the pin table is still
// the gate on any resulting connection.
func (f *Filter) handleDNSResponse(v ipv4View) Verdict {
	payload, ok := v.udpPayload()
	if !ok {
		return pass
	}
	resp := parseDNSResponse(payload)
	if !resp.ok {
		return pass
	}
	if !f.matcher.Match(resp.qname) {
		if nx, ok := nxdomainFrame(v, payload); ok {
			f.reportDeny("blocked DNS for %s (not in allowlist)", resp.qname)
			return Verdict{Action: Replace, ReplacementFrame: nx, Reason: "nxdomain " + resp.qname}
		}
		return f.deny("dropped unparse-rewritable DNS for %s", resp.qname)
	}
	now := f.now()
	f.mu.Lock()
	for _, a := range resp.arecords {
		f.pins.Pin(a.ip, now.Add(clampTTL(a.ttl)))
	}
	f.mu.Unlock()
	return pass
}

// clampTTL converts a DNS TTL (seconds) into a pin lifetime within [minPinTTL,
// maxPinTTL].
func clampTTL(ttlSeconds uint32) time.Duration {
	d := time.Duration(ttlSeconds) * time.Second
	if d < minPinTTL {
		return minPinTTL
	}
	if d > maxPinTTL {
		return maxPinTTL
	}
	return d
}

// PinnedCount reports the number of live pins, for tests and metrics.
func (f *Filter) PinnedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins.Len()
}

// pinned reports whether ip is currently allowed, for tests.
func (f *Filter) pinned(ip netip.Addr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pins.Allowed(ip, f.now())
}

func (f *Filter) deny(format string, args ...any) Verdict {
	f.reportDeny(format, args...)
	return drop
}

func (f *Filter) reportDeny(format string, args ...any) {
	if f.onDeny != nil {
		f.onDeny(fmt.Sprintf(format, args...))
	}
}

func isDHCP(src, dst uint16) bool {
	return (src == dhcpPort1 || src == dhcpPort2) && (dst == dhcpPort1 || dst == dhcpPort2)
}

func ethTypeName(frame []byte) string {
	switch etherType(frame) {
	case ethTypeIPv6:
		return "IPv6"
	case ethTypeARP:
		return "ARP"
	case 0:
		return "runt"
	default:
		return fmt.Sprintf("0x%04x", etherType(frame))
	}
}
