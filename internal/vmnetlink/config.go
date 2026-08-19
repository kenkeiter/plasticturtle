// Package vmnetlink creates a macOS vmnet.framework interface in shared (NAT)
// mode and exposes blocking ethernet frame I/O over it.
//
// The subnet is always passed explicitly. macOS 26 ignores Shared_Net_Address
// in com.apple.vmnet.plist and hands raw vmnet API clients a hardcoded
// 192.168.2.0/24 — which collides with common home LANs — but it does honor
// vmnet_start_address_key/vmnet_end_address_key/vmnet_subnet_mask_key, so the
// caller picks the range and gets what it asked for.
package vmnetlink

import (
	"fmt"
	"net/netip"
)

// Config describes the interface to create. Only the knobs that are known to
// matter for a foreign-MAC guest behind NAT are exposed; everything else (MAC
// allocation, TSO, checksum offload, MTU) is left at the vmnet default, which
// is the combination softnet has run in production with.
type Config struct {
	// Subnet is the IPv4 /24 the interface serves. The host gateway is .1 and
	// the DHCP pool runs .2–.254.
	Subnet netip.Prefix

	// Isolation sets vmnet_enable_isolation_key, which stops guests on this
	// interface from reaching guests on other vmnet interfaces.
	Isolation bool
}

// validate rejects subnets vmnet would either refuse or silently reinterpret.
// The /24-only restriction is deliberate: the gateway/pool derivation below
// assumes a single final octet, and pt has no use for anything wider.
func (c Config) validate() error {
	if !c.Subnet.IsValid() {
		return fmt.Errorf("vmnetlink: subnet is unset")
	}
	if !c.Subnet.Addr().Is4() {
		return fmt.Errorf("vmnetlink: subnet %s is not IPv4", c.Subnet)
	}
	if c.Subnet.Bits() != 24 {
		return fmt.Errorf("vmnetlink: subnet %s is not a /24", c.Subnet)
	}
	if c.Subnet.Masked() != c.Subnet {
		return fmt.Errorf("vmnetlink: subnet %s is not a network base address (want %s)", c.Subnet, c.Subnet.Masked())
	}
	if !c.Subnet.Addr().IsPrivate() {
		return fmt.Errorf("vmnetlink: subnet %s is not inside RFC1918 private space", c.Subnet)
	}
	return nil
}

// subnetParams renders the three vmnet address parameters for a validated /24:
// the gateway the host takes, the last address the DHCP pool may hand out, and
// the mask. .255 is left out of the pool because it is the broadcast address.
func subnetParams(p netip.Prefix) (start, end, mask string) {
	o := p.Addr().As4()
	start = netip.AddrFrom4([4]byte{o[0], o[1], o[2], 1}).String()
	end = netip.AddrFrom4([4]byte{o[0], o[1], o[2], 254}).String()
	return start, end, "255.255.255.0"
}
