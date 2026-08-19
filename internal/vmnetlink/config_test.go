package vmnetlink

import (
	"net/netip"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name   string
		subnet string
		ok     bool
	}{
		{"private /24", "192.168.252.0/24", true},
		{"10/8 private /24", "10.7.3.0/24", true},
		{"172.16 private /24", "172.20.5.0/24", true},
		{"unset", "", false},
		{"not a network base", "192.168.252.1/24", false},
		{"wrong prefix length", "192.168.0.0/16", false},
		{"host route", "192.168.252.5/32", false},
		{"public range", "8.8.8.0/24", false},
		{"carrier-grade NAT is not RFC1918", "100.64.0.0/24", false},
		{"ipv6", "fd00::/24", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if tc.subnet != "" {
				cfg.Subnet = netip.MustParsePrefix(tc.subnet)
			}
			err := cfg.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate(%q) = %v, want nil", tc.subnet, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("validate(%q) = nil, want an error", tc.subnet)
			}
		})
	}
}

func TestConfigValidateRejectsIPv4In6(t *testing.T) {
	// A 4-in-6 prefix would format as a plausible IPv4 address but vmnet wants a
	// real dotted quad, so it must not slip through.
	cfg := Config{Subnet: netip.PrefixFrom(netip.MustParseAddr("::ffff:192.168.252.0"), 24)}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate accepted a 4-in-6 prefix")
	}
}

func TestSubnetParams(t *testing.T) {
	start, end, mask := subnetParams(netip.MustParsePrefix("192.168.252.0/24"))
	if start != "192.168.252.1" {
		t.Errorf("start = %q, want 192.168.252.1", start)
	}
	if end != "192.168.252.254" {
		t.Errorf("end = %q, want 192.168.252.254", end)
	}
	if mask != "255.255.255.0" {
		t.Errorf("mask = %q, want 255.255.255.0", mask)
	}
}
