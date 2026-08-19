package netfw

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// Test frame builders. These construct real wire bytes so the parser and the
// verdict path are exercised against the same encoding a guest would emit.

var (
	guestMAC = [6]byte{0x5e, 0xb8, 0x54, 0x9e, 0x81, 0xfe}
	gwMAC    = [6]byte{0xf2, 0x2f, 0x4b, 0x31, 0x62, 0x64}
	guestIP  = netip.MustParseAddr("192.168.2.2")
	gwIP     = netip.MustParseAddr("192.168.2.1")
)

func ethIPv4(src, dst [6]byte, ipPayload []byte) []byte {
	frame := make([]byte, ethHeaderLen+len(ipPayload))
	copy(frame[0:6], dst[:])
	copy(frame[6:12], src[:])
	binary.BigEndian.PutUint16(frame[12:14], ethTypeIPv4)
	copy(frame[ethHeaderLen:], ipPayload)
	return frame
}

func ipv4Hdr(proto uint8, src, dst netip.Addr, l4 []byte) []byte {
	ip := make([]byte, 20+len(l4))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(l4)))
	ip[8] = 64 // TTL
	ip[9] = proto
	s4, d4 := src.As4(), dst.As4()
	copy(ip[12:16], s4[:])
	copy(ip[16:20], d4[:])
	ip[10], ip[11] = 0, 0
	binary.BigEndian.PutUint16(ip[10:12], onesComplementSum(ip[:20]))
	copy(ip[20:], l4)
	return ip
}

func udpHdr(srcPort, dstPort uint16, payload []byte) []byte {
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(8+len(payload)))
	copy(udp[8:], payload)
	return udp // checksum left 0 (valid: "no checksum" in IPv4)
}

func tcpSYN(srcPort, dstPort uint16) []byte {
	tcp := make([]byte, 20)
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	tcp[12] = 0x50 // data offset 5
	tcp[13] = 0x02 // SYN
	return tcp
}

// udpFrame is guest->server or server->guest UDP.
func udpFrame(smac, dmac [6]byte, sip, dip netip.Addr, sport, dport uint16, payload []byte) []byte {
	return ethIPv4(smac, dmac, ipv4Hdr(protoUDP, sip, dip, udpHdr(sport, dport, payload)))
}

func tcpFrame(smac, dmac [6]byte, sip, dip netip.Addr, sport, dport uint16) []byte {
	return ethIPv4(smac, dmac, ipv4Hdr(protoTCP, sip, dip, tcpSYN(sport, dport)))
}

// tcpAckFrame is like tcpFrame but with the ACK flag set — reply traffic of an
// established connection rather than a new one.
func tcpAckFrame(smac, dmac [6]byte, sip, dip netip.Addr, sport, dport uint16) []byte {
	tcp := tcpSYN(sport, dport)
	tcp[13] = 0x10 // ACK
	return ethIPv4(smac, dmac, ipv4Hdr(protoTCP, sip, dip, tcp))
}

// dhcpAckPayload packs a minimal DHCPACK naming router/DNS/server-identifier
// options, the wire shape vmnet's bootpd hands a guest.
func dhcpAckPayload(router, dns, serverID netip.Addr) []byte {
	p := make([]byte, bootpOptionsOff)
	p[0] = bootpReply
	p = append(p, dhcpMagicCookie[:]...)
	p = append(p, 53, 1, 5) // message type: ACK
	r4, d4, s4 := router.As4(), dns.As4(), serverID.As4()
	p = append(p, 3, 4)
	p = append(p, r4[:]...)
	p = append(p, 6, 4)
	p = append(p, d4[:]...)
	p = append(p, 54, 4)
	p = append(p, s4[:]...)
	return append(p, 255)
}

// dnsQuery packs an A-record query for name.
func dnsQuery(t *testing.T, id uint16, name string) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  dnsmessage.MustNewName(fqdn(name)),
			Type:  dnsmessage.TypeA,
			Class: dnsmessage.ClassINET,
		}},
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack query: %v", err)
	}
	return b
}

// dnsResponseA packs a response for name carrying the given A records (ip,ttl),
// optionally with a CNAME owner to model a CDN indirection.
func dnsResponseA(t *testing.T, id uint16, name string, ips []arec) []byte {
	t.Helper()
	q := dnsmessage.Question{
		Name:  dnsmessage.MustNewName(fqdn(name)),
		Type:  dnsmessage.TypeA,
		Class: dnsmessage.ClassINET,
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, Response: true, RecursionAvailable: true},
		Questions: []dnsmessage.Question{q},
	}
	for _, a := range ips {
		msg.Answers = append(msg.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   a.ttl,
			},
			Body: &dnsmessage.AResource{A: a.ip.As4()},
		})
	}
	b, err := msg.Pack()
	if err != nil {
		t.Fatalf("pack response: %v", err)
	}
	return b
}

func fqdn(s string) string {
	if len(s) == 0 || s[len(s)-1] != '.' {
		return s + "."
	}
	return s
}
