package netfw

import (
	"encoding/binary"
	"net/netip"

	"golang.org/x/net/dns/dnsmessage"
)

// Layer-2/3/4 constants. Only the subset the filter reasons about is named.
const (
	ethHeaderLen = 14
	ethTypeIPv4  = 0x0800
	ethTypeARP   = 0x0806
	ethTypeIPv6  = 0x86DD

	protoICMP = 1
	protoTCP  = 6
	protoUDP  = 17

	dnsPort   = 53
	dhcpPort1 = 67
	dhcpPort2 = 68
)

// etherType returns the EtherType of a frame, or 0 if it is too short. VLAN
// tags are not expected on the vmnet link and are treated as "not IPv4", i.e.
// dropped under a restricted policy.
func etherType(frame []byte) uint16 {
	if len(frame) < ethHeaderLen {
		return 0
	}
	return binary.BigEndian.Uint16(frame[12:14])
}

// ipv4View is a parsed, bounds-checked view over an IPv4 frame. Offsets index
// into the original frame slice so a rewrite can edit in place.
type ipv4View struct {
	frame    []byte
	ihl      int // IPv4 header length in bytes
	l4       int // offset of the L4 header
	proto    uint8
	src, dst netip.Addr
}

// parseIPv4 validates enough of an Ethernet+IPv4 frame to rule on it. It returns
// ok=false for anything malformed or truncated; the caller treats that as
// undecodable and drops it, which is the safe default for a hostile guest.
func parseIPv4(frame []byte) (ipv4View, bool) {
	if etherType(frame) != ethTypeIPv4 {
		return ipv4View{}, false
	}
	ip := frame[ethHeaderLen:]
	if len(ip) < 20 {
		return ipv4View{}, false
	}
	if ip[0]>>4 != 4 {
		return ipv4View{}, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || ihl > len(ip) {
		return ipv4View{}, false
	}
	totalLen := int(binary.BigEndian.Uint16(ip[2:4]))
	// The frame may carry Ethernet padding beyond the IP payload; totalLen must
	// fit within it but need not equal it.
	if totalLen < ihl || totalLen > len(ip) {
		return ipv4View{}, false
	}
	// Fragmented packets (nonzero fragment offset, or MF set) cannot be ruled on
	// by port/DNS here; reject them rather than guess. The guest's normal DNS
	// and TCP to allowed hosts is not fragmented.
	flagsFrag := binary.BigEndian.Uint16(ip[6:8])
	if flagsFrag&0x2000 != 0 || flagsFrag&0x1fff != 0 {
		return ipv4View{}, false
	}
	v := ipv4View{
		frame: frame,
		ihl:   ihl,
		l4:    ethHeaderLen + ihl,
		proto: ip[9],
		src:   netip.AddrFrom4([4]byte(ip[12:16])),
		dst:   netip.AddrFrom4([4]byte(ip[16:20])),
	}
	return v, true
}

// l4Ports returns the source and destination ports for TCP/UDP, or ok=false if
// the L4 header is truncated or the protocol has no ports.
func (v ipv4View) l4Ports() (src, dst uint16, ok bool) {
	if v.proto != protoTCP && v.proto != protoUDP {
		return 0, 0, false
	}
	if len(v.frame) < v.l4+4 {
		return 0, 0, false
	}
	src = binary.BigEndian.Uint16(v.frame[v.l4 : v.l4+2])
	dst = binary.BigEndian.Uint16(v.frame[v.l4+2 : v.l4+4])
	return src, dst, true
}

// udpPayload returns the UDP payload slice (aliasing the frame) or ok=false.
func (v ipv4View) udpPayload() ([]byte, bool) {
	if v.proto != protoUDP || len(v.frame) < v.l4+8 {
		return nil, false
	}
	udpLen := int(binary.BigEndian.Uint16(v.frame[v.l4+4 : v.l4+6]))
	if udpLen < 8 {
		return nil, false
	}
	start := v.l4 + 8
	end := v.l4 + udpLen
	if end > len(v.frame) || end < start {
		return nil, false
	}
	return v.frame[start:end], true
}

// arec is one address answer harvested from a DNS response.
type arec struct {
	ip  netip.Addr
	ttl uint32
}

// dnsResponse describes a parsed DNS response relevant to the filter: the name
// that was asked, and every A record in the answer. AAAA records are noted only
// by count (the filter drops IPv6 traffic regardless, so their addresses are not
// pinned). ok is false for a query, a malformed message, or a response with no
// question.
type dnsResponse struct {
	id       uint16
	qname    string
	arecords []arec
	ok       bool
}

// parseDNSResponse decodes a DNS message and, if it is a response, extracts the
// question name and A records. It tolerates partial answers: a record that fails
// to parse ends harvesting but does not discard what was already read, so a
// truncated-but-valid prefix still pins what it named.
func parseDNSResponse(payload []byte) dnsResponse {
	var p dnsmessage.Parser
	hdr, err := p.Start(payload)
	if err != nil || !hdr.Response {
		return dnsResponse{}
	}
	q, err := p.Question()
	if err != nil {
		return dnsResponse{}
	}
	if err := p.SkipAllQuestions(); err != nil {
		return dnsResponse{}
	}
	res := dnsResponse{id: hdr.ID, qname: trimDot(q.Name.String()), ok: true}
	for {
		ah, err := p.AnswerHeader()
		if err != nil {
			break // includes ErrSectionDone
		}
		switch ah.Type {
		case dnsmessage.TypeA:
			r, err := p.AResource()
			if err != nil {
				return res
			}
			res.arecords = append(res.arecords, arec{ip: netip.AddrFrom4(r.A), ttl: ah.TTL})
		default:
			if err := p.SkipAnswer(); err != nil {
				return res
			}
		}
	}
	return res
}

// buildNXDOMAIN packs a minimal NXDOMAIN response echoing the request's ID and
// question. It is used to answer disallowed names so the guest's resolver gets
// an authoritative-looking negative answer instead of silence.
func buildNXDOMAIN(id uint16, question dnsmessage.Question) ([]byte, error) {
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 id,
			Response:           true,
			RecursionDesired:   true,
			RecursionAvailable: true,
			RCode:              dnsmessage.RCodeNameError,
		},
		Questions: []dnsmessage.Question{question},
	}
	return msg.Pack()
}

func trimDot(s string) string {
	if n := len(s); n > 0 && s[n-1] == '.' {
		return s[:n-1]
	}
	return s
}
