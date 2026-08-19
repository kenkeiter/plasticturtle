package netfw

import (
	"encoding/binary"

	"golang.org/x/net/dns/dnsmessage"
)

// onesComplementSum computes the 16-bit ones-complement checksum of b, the
// primitive both the IPv4 header and UDP checksums are built from.
func onesComplementSum(bs ...[]byte) uint16 {
	var sum uint32
	var carryByte int = -1 // a leftover odd byte from the previous slice
	for _, b := range bs {
		i := 0
		if carryByte >= 0 && len(b) > 0 {
			sum += uint32(carryByte)<<8 | uint32(b[0])
			i = 1
			carryByte = -1
		}
		for ; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if i < len(b) {
			carryByte = int(b[i])
		}
	}
	if carryByte >= 0 {
		sum += uint32(carryByte) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// rewriteDNSPayload returns a new frame identical to v's frame through the
// Ethernet, IPv4, and UDP headers, but carrying newPayload as the UDP payload,
// with the IPv4 total-length, IPv4 header checksum, UDP length, and UDP checksum
// all recomputed. The original frame is not modified. v must be a UDP view.
func rewriteDNSPayload(v ipv4View, newPayload []byte) []byte {
	ipStart := ethHeaderLen
	udpStart := v.l4
	newUDPLen := 8 + len(newPayload)
	newTotalLen := v.ihl + newUDPLen

	out := make([]byte, udpStart+newUDPLen)
	// Ethernet + IPv4 header + UDP header, copied verbatim, then patched.
	copy(out, v.frame[:udpStart+8])
	copy(out[udpStart+8:], newPayload)

	ip := out[ipStart:]
	binary.BigEndian.PutUint16(ip[2:4], uint16(newTotalLen))
	ip[10], ip[11] = 0, 0
	sum := onesComplementSum(ip[:v.ihl])
	binary.BigEndian.PutUint16(ip[10:12], sum)

	udp := out[udpStart:]
	binary.BigEndian.PutUint16(udp[4:6], uint16(newUDPLen))
	udp[6], udp[7] = 0, 0
	// UDP checksum covers a pseudo-header (src, dst, zero, proto, udp length)
	// plus the UDP header and payload.
	var pseudo [12]byte
	copy(pseudo[0:4], ip[12:16])
	copy(pseudo[4:8], ip[16:20])
	pseudo[8] = 0
	pseudo[9] = protoUDP
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(newUDPLen))
	csum := onesComplementSum(pseudo[:], udp[:newUDPLen])
	// A computed UDP checksum of zero is transmitted as all-ones; zero is
	// reserved to mean "no checksum".
	if csum == 0 {
		csum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], csum)
	return out
}

// nxdomainFrame rebuilds v (a DNS response frame) as an NXDOMAIN answer for the
// same question. It returns ok=false if the payload cannot be re-parsed into a
// question, in which case the caller drops the frame instead.
func nxdomainFrame(v ipv4View, payload []byte) ([]byte, bool) {
	var p dnsmessage.Parser
	hdr, err := p.Start(payload)
	if err != nil {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		return nil, false
	}
	nx, err := buildNXDOMAIN(hdr.ID, q)
	if err != nil {
		return nil, false
	}
	return rewriteDNSPayload(v, nx), true
}
