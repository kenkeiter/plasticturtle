// Command pt-softnet-shim is Plastic Turtle's domain firewall and the sandbox's
// entire software-networking layer, packaged as a drop-in replacement for the
// `softnet` binary that Tart spawns.
//
// Tart resolves "softnet" from its PATH and hands the new process the guest's
// virtual network link as stdin (a SOCK_DGRAM socketpair, one datagram per
// ethernet frame). pt puts this shim ahead of anything else on that PATH. The
// shim creates the guest's NAT network itself through vmnet.framework
// (internal/vmnetlink) and relays frames between that interface and stdin —
// applying an internal/netfw filter that enforces the project's domain allowlist
// by DNS-pinning. With no policy (or an "open" one) it relays untouched, so it
// is safe to sit in the path unconditionally. There is no external softnet.
//
// Privilege: creating a vmnet interface requires root, so this binary is
// installed setuid-root. Root is used for exactly one thing — vmnetlink.Open —
// after which the interface belongs to the process rather than to the uid, and
// the long-lived relay drops back to the invoking user. Nothing
// user-controllable (the policy file, guest frames) is read while privileged.
package main

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/netfw"
	"github.com/kenkeiter/plasticturtle/internal/vmnetlink"
)

// defaultSubnet is used when PT_NETFW_SUBNET is unset, which only happens on a
// direct invocation: pt always pins the subnet it picked. It is the top of the
// range pt scans, so a manual run lands where a pt-launched sandbox would.
const defaultSubnet = "192.168.252.0/24"

func main() {
	lg := newLogger(os.Getenv("PT_SHIM_LOG"))
	lg.printf("start argv=%v euid=%d ruid=%d", os.Args[1:], os.Geteuid(), os.Getuid())

	if err := run(lg); err != nil {
		lg.printf("fatal: %v", err)
		fmt.Fprintln(os.Stderr, "pt-softnet-shim:", err)
		os.Exit(1)
	}
}

func run(lg *logger) error {
	// argv carries no privilege of its own, so parsing it first costs nothing and
	// makes a malformed guest MAC fail before we ask vmnet for an interface.
	guestMAC, err := parseVMMAC(os.Args[1:])
	if err != nil {
		return err
	}

	subnet, err := subnetFromEnv(os.Getenv("PT_NETFW_SUBNET"))
	if err != nil {
		return err
	}

	// The one privileged step. Isolation keeps this sandbox off any other vmnet
	// interface's guests, which no policy of ours would otherwise cover.
	link, err := vmnetlink.Open(vmnetlink.Config{Subnet: subnet, Isolation: true})
	if err != nil {
		if os.Geteuid() != 0 {
			return fmt.Errorf("%w\nthe shim must be installed setuid-root; run `pt setup`", err)
		}
		return err
	}
	defer link.Close()

	// Root is no longer needed: the interface outlives the privilege. Drop before
	// the long-lived relay, and before reading the policy file or a single guest
	// frame.
	if err := dropPrivileges(); err != nil {
		return fmt.Errorf("dropping privileges: %w", err)
	}
	lg.printf("privileges dropped to uid=%d gid=%d", os.Getuid(), os.Getgid())

	macDesc := "off (no --vm-mac-address)"
	if guestMAC != nil {
		macDesc = guestMAC.String()
	}
	lg.printf("vmnet interface: subnet=%s gateway=%s max-packet=%d mac-enforcement=%s",
		subnet, link.Gateway(), link.MaxPacketSize(), macDesc)

	filter, desc := buildFilter(lg)
	lg.printf("filter: %s", desc)

	var egress, ingress stats
	var egressDecide, ingressDecide verdictFunc
	if filter != nil {
		egressDecide = filter.EgressFromGuest
		ingressDecide = filter.IngressToGuest
	}
	if guestMAC != nil {
		egressDecide = enforceSourceMAC(guestMAC, egressDecide, &egress, lg)
	}

	// Registered before any frame moves so a stop that arrives during the first
	// moments of the relay is handled by us rather than by the default
	// disposition, which would kill the guest's network without a word.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)

	guest := datagramLink{os.Stdin} // the link Tart handed us
	errc := make(chan error, 2)
	go func() { errc <- relayFiltered(link, guest, egressDecide, &egress) }()
	go func() { errc <- relayFiltered(guest, link, ingressDecide, &ingress) }()

	stop := make(chan struct{})
	go logStats(lg, &egress, &ingress, stop)

	// Either end going quiet is a shutdown: Tart closing stdin means the VM is
	// gone, and a link error means we have no network to relay onto. Closing the
	// link unblocks the other direction's parked read.
	reason := waitForShutdown(errc, sigc, lg)
	close(stop)
	if err := link.Close(); err != nil {
		lg.printf("closing vmnet interface: %v", err)
	}

	lg.printf("shutdown (%s); egress frames=%d passed=%d dropped=%d rewrote=%d mac-dropped=%d; ingress frames=%d passed=%d dropped=%d rewrote=%d",
		reason,
		egress.frames.Load(), egress.passed.Load(), egress.dropped.Load(), egress.rewrote.Load(), egress.macDropped.Load(),
		ingress.frames.Load(), ingress.passed.Load(), ingress.dropped.Load(), ingress.rewrote.Load())
	return nil
}

// waitForShutdown blocks until a relay direction ends or Tart signals us, and
// returns a short description for the log. A signal is a clean stop: Tart stops
// softnet with SIGINT, and the caller closes the link, which ends both relay
// goroutines. Relay errors are logged rather than returned — by the time either
// side has failed there is nothing left to do but exit cleanly, and a nonzero
// exit from "softnet" would be reported by Tart as a boot failure.
func waitForShutdown(errc <-chan error, sigc <-chan os.Signal, lg *logger) string {
	select {
	case err := <-errc:
		if err != nil {
			lg.printf("relay ended: %v", err)
			return "relay error"
		}
		return "link closed"
	case s := <-sigc:
		return fmt.Sprintf("signal %v", s)
	}
}

// subnetFromEnv parses PT_NETFW_SUBNET ("a.b.c.0/24"), falling back to
// defaultSubnet when unset. A value that is set but malformed is fatal: pt
// passes the subnet it verified free of host collisions, so a wrong one would
// silently put the sandbox on a range that breaks host connectivity. Shape is
// checked here as well as inside vmnetlink so a bad value fails before the
// process asks for root.
func subnetFromEnv(v string) (netip.Prefix, error) {
	raw := v
	if raw == "" {
		raw = defaultSubnet
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("PT_NETFW_SUBNET=%q is not a CIDR prefix: %w", v, err)
	}
	if !p.Addr().Is4() || p.Bits() != 24 || p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("PT_NETFW_SUBNET=%q must be an IPv4 /24 network address such as %s", v, defaultSubnet)
	}
	return p, nil
}

// parseVMMAC extracts the guest's MAC from Tart's `--vm-mac-address <mac>` (or
// `--vm-mac-address=<mac>`). Everything else in argv is ignored: Tart also
// passes --vm-fd and friends, which described the old external softnet's view of
// the link and mean nothing now that the link is stdin. A missing flag disables
// enforcement (direct invocation); a present but unparsable one is fatal, since
// the alternative is silently relaying frames we were told to police.
func parseVMMAC(argv []string) (net.HardwareAddr, error) {
	const flag = "--vm-mac-address"
	raw := ""
	for i, a := range argv {
		switch {
		case a == flag && i+1 < len(argv):
			raw = argv[i+1]
		case strings.HasPrefix(a, flag+"="):
			raw = strings.TrimPrefix(a, flag+"=")
		}
	}
	if raw == "" {
		return nil, nil
	}
	mac, err := net.ParseMAC(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not a MAC address: %w", flag, raw, err)
	}
	return mac, nil
}

// buildFilter reads the policy file named by PT_NETFW_POLICY and returns a
// Filter, or (nil, reason) for pass-through. A policy that is absent, open, or
// unreadable yields pass-through: this binary never fails a boot over its own
// filtering, it only ever adds restriction when explicitly and correctly asked.
func buildFilter(lg *logger) (*netfw.Filter, string) {
	path := os.Getenv("PT_NETFW_POLICY")
	if path == "" {
		return nil, "pass-through (no PT_NETFW_POLICY)"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("pass-through (policy unreadable: %v)", err)
	}
	doc, err := netfw.ParsePolicy(raw)
	if err != nil {
		return nil, fmt.Sprintf("pass-through (policy invalid: %v)", err)
	}
	if !doc.Restricted() {
		return nil, fmt.Sprintf("pass-through (policy %q)", doc.Policy)
	}
	f := netfw.New(netfw.Config{
		Allow:  doc.Allow,
		OnDeny: func(reason string) { lg.printf("DENY %s", reason) },
	})
	return f, fmt.Sprintf("restricted, %d allow patterns", len(doc.Allow))
}

func logStats(lg *logger, egress, ingress *stats, stop <-chan struct{}) {
	if !lg.enabled() {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			lg.printf("egress frames=%d dropped=%d rewrote=%d mac-dropped=%d | ingress frames=%d dropped=%d rewrote=%d",
				egress.frames.Load(), egress.dropped.Load(), egress.rewrote.Load(), egress.macDropped.Load(),
				ingress.frames.Load(), ingress.dropped.Load(), ingress.rewrote.Load())
		}
	}
}
