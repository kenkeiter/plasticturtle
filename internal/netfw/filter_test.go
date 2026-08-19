package netfw

import (
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// newTestFilter builds a filter with a controllable clock and a deny-reason
// capture.
func newTestFilter(t *testing.T, allow ...string) (*Filter, *time.Time, *[]string) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	var denials []string
	f := New(Config{
		Allow:  allow,
		Now:    func() time.Time { return now },
		OnDeny: func(r string) { denials = append(denials, r) },
	})
	return f, &now, &denials
}

func TestFilterPinsAllowedDNSThenPermitsTCP(t *testing.T) {
	f, _, _ := newTestFilter(t, "github.com")
	serverIP := netip.MustParseAddr("140.82.113.4")

	// Guest asks for github.com — outbound query passes.
	q := udpFrame(guestMAC, gwMAC, guestIP, gwIP, 34000, dnsPort, dnsQuery(t, 1, "github.com"))
	if v := f.EgressFromGuest(q); v.Action != Pass {
		t.Fatalf("DNS query egress = %v, want Pass", v.Action)
	}

	// Before any answer, TCP to the server is blocked.
	syn := tcpFrame(guestMAC, gwMAC, guestIP, serverIP, 40000, 443)
	if v := f.EgressFromGuest(syn); v.Action != Drop {
		t.Fatalf("pre-resolution TCP = %v, want Drop", v.Action)
	}

	// Response for github.com pins the server IP.
	resp := udpFrame(gwMAC, guestMAC, gwIP, guestIP, dnsPort, 34000,
		dnsResponseA(t, 1, "github.com", []arec{{ip: serverIP, ttl: 300}}))
	if v := f.IngressToGuest(resp); v.Action != Pass {
		t.Fatalf("allowed DNS response = %v, want Pass", v.Action)
	}
	if !f.pinned(serverIP) {
		t.Fatal("server IP should be pinned after allowed response")
	}

	// Now the same TCP SYN is permitted, and its return traffic too.
	if v := f.EgressFromGuest(syn); v.Action != Pass {
		t.Fatalf("post-resolution TCP egress = %v, want Pass", v.Action)
	}
	synack := tcpFrame(gwMAC, guestMAC, serverIP, guestIP, 443, 40000)
	if v := f.IngressToGuest(synack); v.Action != Pass {
		t.Fatalf("return traffic from pinned server = %v, want Pass", v.Action)
	}
}

func TestFilterRewritesDisallowedDNSToNXDOMAIN(t *testing.T) {
	f, _, denials := newTestFilter(t, "github.com")
	badIP := netip.MustParseAddr("203.0.113.9")
	resp := udpFrame(gwMAC, guestMAC, gwIP, guestIP, dnsPort, 34000,
		dnsResponseA(t, 7, "tracker.evil.com", []arec{{ip: badIP, ttl: 300}}))

	v := f.IngressToGuest(resp)
	if v.Action != Replace {
		t.Fatalf("disallowed DNS = %v, want Replace", v.Action)
	}
	if f.pinned(badIP) {
		t.Fatal("disallowed answer must not pin its IP")
	}
	// The replacement must be a well-formed NXDOMAIN for the same question.
	rv, ok := parseIPv4(v.ReplacementFrame)
	if !ok {
		t.Fatal("replacement frame is not parseable IPv4")
	}
	payload, ok := rv.udpPayload()
	if !ok {
		t.Fatal("replacement has no UDP payload")
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(payload)
	if err != nil {
		t.Fatalf("replacement DNS unparseable: %v", err)
	}
	if hdr.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("replacement RCode = %v, want NXDOMAIN", hdr.RCode)
	}
	if hdr.ID != 7 {
		t.Fatalf("replacement ID = %d, want 7 (echo request)", hdr.ID)
	}
	q, err := p.Question()
	if err != nil {
		t.Fatalf("replacement has no question: %v", err)
	}
	if got := trimDot(q.Name.String()); got != "tracker.evil.com" {
		t.Fatalf("replacement question = %q, want tracker.evil.com", got)
	}
	if len(*denials) == 0 {
		t.Fatal("a disallowed DNS response should have reported a denial")
	}
}

func TestFilterChecksumsAreValid(t *testing.T) {
	f, _, _ := newTestFilter(t, "github.com")
	resp := udpFrame(gwMAC, guestMAC, gwIP, guestIP, dnsPort, 34000,
		dnsResponseA(t, 7, "tracker.evil.com", nil))
	v := f.IngressToGuest(resp)
	if v.Action != Replace {
		t.Fatalf("want Replace, got %v", v.Action)
	}
	rv, ok := parseIPv4(v.ReplacementFrame)
	if !ok {
		t.Fatal("replacement not parseable")
	}
	// IPv4 header checksum over the header must be zero (valid).
	ip := v.ReplacementFrame[ethHeaderLen:]
	if s := onesComplementSum(ip[:rv.ihl]); s != 0 {
		t.Fatalf("IPv4 header checksum invalid: residual %#04x", s)
	}
	// UDP checksum over pseudo-header + UDP must be zero.
	udp := v.ReplacementFrame[rv.l4:]
	var pseudo [12]byte
	copy(pseudo[0:4], ip[12:16])
	copy(pseudo[4:8], ip[16:20])
	pseudo[9] = protoUDP
	udpLen := len(udp)
	pseudo[10] = byte(udpLen >> 8)
	pseudo[11] = byte(udpLen)
	if s := onesComplementSum(pseudo[:], udp); s != 0 {
		t.Fatalf("UDP checksum invalid: residual %#04x", s)
	}
}

func TestFilterDropsIPv6AndUnknown(t *testing.T) {
	f, _, _ := newTestFilter(t, "github.com")
	// Minimal IPv6 ethernet frame.
	v6 := make([]byte, ethHeaderLen+40)
	v6[12], v6[13] = 0x86, 0xDD
	if v := f.EgressFromGuest(v6); v.Action != Drop {
		t.Fatalf("IPv6 egress = %v, want Drop", v.Action)
	}
	if v := f.IngressToGuest(v6); v.Action != Drop {
		t.Fatalf("IPv6 ingress = %v, want Drop", v.Action)
	}
}

func TestFilterPassesARPAndDHCP(t *testing.T) {
	f, _, _ := newTestFilter(t)
	arp := make([]byte, ethHeaderLen+28)
	arp[12], arp[13] = 0x08, 0x06
	if v := f.EgressFromGuest(arp); v.Action != Pass {
		t.Fatalf("ARP egress = %v, want Pass", v.Action)
	}
	// DHCP DISCOVER: UDP 68 -> 67 to broadcast.
	bcast := netip.MustParseAddr("255.255.255.255")
	dhcp := udpFrame(guestMAC, gwMAC, netip.IPv4Unspecified(), bcast, 68, 67, []byte("boot"))
	if v := f.EgressFromGuest(dhcp); v.Action != Pass {
		t.Fatalf("DHCP egress = %v, want Pass", v.Action)
	}
}

func TestFilterPinExpiresThenReblocks(t *testing.T) {
	f, now, _ := newTestFilter(t, "github.com")
	serverIP := netip.MustParseAddr("140.82.113.4")
	resp := udpFrame(gwMAC, guestMAC, gwIP, guestIP, dnsPort, 34000,
		dnsResponseA(t, 1, "github.com", []arec{{ip: serverIP, ttl: 60}}))
	f.IngressToGuest(resp)

	syn := tcpFrame(guestMAC, gwMAC, guestIP, serverIP, 40000, 443)
	if v := f.EgressFromGuest(syn); v.Action != Pass {
		t.Fatal("should pass while pinned")
	}
	*now = now.Add(61 * time.Second) // past the pin (ttl 60 within [30s,10m])
	if v := f.EgressFromGuest(syn); v.Action != Drop {
		t.Fatalf("post-expiry TCP = %v, want Drop", v.Action)
	}
}

func TestClampTTL(t *testing.T) {
	cases := []struct {
		ttl  uint32
		want time.Duration
	}{
		{0, minPinTTL},
		{5, minPinTTL},
		{300, 300 * time.Second},
		{100000, maxPinTTL},
	}
	for _, c := range cases {
		if got := clampTTL(c.ttl); got != c.want {
			t.Errorf("clampTTL(%d) = %v, want %v", c.ttl, got, c.want)
		}
	}
}
