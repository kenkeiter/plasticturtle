package main

import (
	"net"
	"testing"
)

func TestSubnetFromEnvDefaultsWhenUnset(t *testing.T) {
	got, err := subnetFromEnv("")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != defaultSubnet {
		t.Fatalf("default subnet = %s, want %s", got, defaultSubnet)
	}
}

func TestSubnetFromEnvAcceptsPinnedSubnet(t *testing.T) {
	got, err := subnetFromEnv("192.168.231.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.168.231.0/24" {
		t.Fatalf("subnet = %s", got)
	}
}

// A malformed subnet must fail the shim, never fall back to the default: pt
// passes the range it verified free of host collisions, and quietly using a
// different one puts the sandbox on top of a real network.
func TestSubnetFromEnvRejectsBadValues(t *testing.T) {
	for _, v := range []string{
		"nonsense",
		"192.168.252.0",     // no prefix length
		"192.168.252.0/16",  // not a /24
		"192.168.252.7/24",  // not a network base address
		"fd00::/24",         // not IPv4
		" 192.168.252.0/24", // stray whitespace
	} {
		if got, err := subnetFromEnv(v); err == nil {
			t.Fatalf("subnetFromEnv(%q) = %s, want an error", v, got)
		}
	}
}

func TestParseVMMACFlagForms(t *testing.T) {
	want := "aa:bb:cc:dd:ee:ff"
	for _, argv := range [][]string{
		{"--vm-fd", "3", "--vm-mac-address", want, "--vm-net-type", "shared"},
		{"--vm-mac-address=" + want},
	} {
		got, err := parseVMMAC(argv)
		if err != nil {
			t.Fatalf("argv %v: %v", argv, err)
		}
		if got == nil || got.String() != want {
			t.Fatalf("argv %v: mac = %v, want %s", argv, got, want)
		}
	}
}

// Without the flag there is no MAC to enforce — a direct invocation, not a Tart
// one — and enforcement is simply off.
func TestParseVMMACAbsent(t *testing.T) {
	got, err := parseVMMAC([]string{"--vm-fd", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("mac = %v, want none", got)
	}
}

func TestParseVMMACRejectsGarbage(t *testing.T) {
	if _, err := parseVMMAC([]string{"--vm-mac-address", "not-a-mac"}); err == nil {
		t.Fatal("unparsable MAC accepted")
	}
}

// The datagram side reports the buffer size the relay has always used for the
// socket Tart hands us, independent of what the vmnet side reports.
func TestDatagramLinkMaxPacketSize(t *testing.T) {
	if got := (datagramLink{}).MaxPacketSize(); got != maxFrame {
		t.Fatalf("datagram max packet = %d, want %d", got, maxFrame)
	}
}

func TestHasSourceMAC(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	if !hasSourceMAC(ethFrame(mac, "x"), mac) {
		t.Fatal("guest's own MAC rejected")
	}
	if hasSourceMAC(ethFrame(mac, "x")[:12], mac) {
		t.Fatal("frame with a truncated ethernet header accepted")
	}
}
